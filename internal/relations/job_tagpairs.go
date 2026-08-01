package relations

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

// Tag-similarity candidates: the second detector feeding the pair
// queue. pHash finds near-identical pixels; this finds recolours,
// redraws and "based on" work, which share tags but no pixels.

// Queue sources. A pair both detectors nominate carries `both`, which
// is the strongest prior the queue can offer; `review` marks a pair the
// operator asked to see again rather than one a detector found.
const (
	SourcePhash  = "phash"
	SourceTags   = "tags"
	SourceBoth   = "both"
	SourceReview = "review"
)

// Tag scores map into the distance column so one ordering key serves
// both detectors. The base sits above the phash budget's ceiling (12)
// so the bands never overlap: smallest-distance-first drains every
// pixel match, then walks tag matches strongest first. Distance 0 stays
// reserved for an operator requeue, the only row that should jump the
// whole queue.
const (
	tagPairDistanceBase = 16
	tagPairDistanceSpan = 48
)

// tagPairTopK caps how many matches one image contributes. Tag
// similarity is cluster-shaped - two hundred same-character images
// pairwise-match - and without the cap one cluster buries everything
// else in the queue.
const tagPairTopK = 3

// tagPairMinShared is how many counted tags two images must have in
// common before the queue will offer the pair, whatever they score.
// Measured against a booru library's own declared duplicates: pairs
// under this floor are almost never related, because a handful of rare
// tags can carry a high score between two images that share nothing
// else. A search can afford that noise since the operator is looking;
// a work queue cannot.
const tagPairMinShared = 10

// TagPairDistance maps a score into the queue's ordering key.
func TagPairDistance(score float64) int {
	switch {
	case score > 1:
		score = 1
	case score < 0:
		score = 0
	}
	return tagPairDistanceBase + int(math.Round((1-score)*tagPairDistanceSpan))
}

// tagPairCandidate is one admitted match for a seed image.
type tagPairCandidate struct {
	imageID int64
	score   float64
}

// findTagPairs scores every eligible pair over an in-memory tag index
// and queues each image's best matches. Scoring per seed through SQL
// would visit each tag's posting once per image carrying it - a cost
// quadratic in tag usage - so the pass bulk-loads the index once and
// lets it die with the run. Cancellable like the phash walk; the pass
// is idempotent, so a re-walk only re-confirms what is already there.
func findTagPairs(ctx context.Context, database *db.DB, threshold float64, progress FindPairsProgress) (int, error) {
	// A pair needs tagPairMinShared counted tags in common, and counted
	// tags never outnumber the raw column, so anything under that floor
	// cannot form an admissible pair and stays out of the index.
	corpus, err := tags.LoadSimilarityCorpus(database, tagPairMinShared)
	if err != nil {
		return 0, fmt.Errorf("load tag-pair corpus: %w", err)
	}
	postings := buildPostings(corpus)
	matches := make([][]tagPairCandidate, len(corpus))
	scan := newPairScan(len(corpus))
	for i := range corpus {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if progress != nil && i%64 == 0 {
			progress(i, len(corpus), "tag probing")
		}
		scorePairsFrom(corpus, postings, i, threshold, matches, scan)
	}
	added := 0
	// Chunked commits, like the phash walk's flush: one WAL write per
	// admitted candidate turns a big library's queue fill into thousands
	// of tiny transactions.
	const txChunk = 500
	var pending []tagPairInsert
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := database.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, p := range pending {
			landed, err := storeTagPairTx(tx, p)
			if err != nil {
				return err
			}
			if landed {
				added++
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for i := range corpus {
		for _, c := range matches[i] {
			if ctx.Err() != nil {
				return added, ctx.Err()
			}
			p, ok, err := admitTagPair(ctx, database, corpus[i].ID, c)
			if err != nil {
				return added, err
			}
			if !ok {
				continue
			}
			pending = append(pending, p)
			if len(pending) >= txChunk {
				if err := flush(); err != nil {
					return added, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return added, err
	}
	if progress != nil {
		progress(len(corpus), len(corpus), "tag probing")
	}
	return added, nil
}

// tagPostings is the corpus inverted by tag in one flat array: entries
// holds every carrier index back to back, offsets says where each tag's
// run starts. A map of per-tag slices costs one allocation per tag and
// scatters the walk across the heap; the runs here are contiguous.
type tagPostings struct {
	offsets []int32
	entries []int32
}

func (p *tagPostings) carriers(tagID int32) []int32 {
	if tagID < 0 || int(tagID)+1 >= len(p.offsets) {
		return nil
	}
	return p.entries[p.offsets[tagID]:p.offsets[tagID+1]]
}

// scorable reports whether an image can take part in an admissible
// pair at all: the shared-tag floor counts seeding tags, so an image
// carrying fewer than that can never clear it, whichever side it is on.
// Keeping those out of the index removes them as candidates too.
func scorable(img *tags.SimilarityCorpusImage) bool {
	n := 0
	for _, t := range img.Tags {
		if t.Seeds {
			n++
			if n >= tagPairMinShared {
				return true
			}
		}
	}
	return false
}

func buildPostings(corpus []tags.SimilarityCorpusImage) *tagPostings {
	var maxTag int32
	total := 0
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if !t.Seeds {
				continue
			}
			if t.TagID > maxTag {
				maxTag = t.TagID
			}
			total++
		}
	}
	p := &tagPostings{offsets: make([]int32, maxTag+2), entries: make([]int32, total)}
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if t.Seeds {
				p.offsets[t.TagID+1]++
			}
		}
	}
	for k := 1; k < len(p.offsets); k++ {
		p.offsets[k] += p.offsets[k-1]
	}
	fill := make([]int32, len(p.offsets))
	copy(fill, p.offsets)
	for i := range corpus {
		if !scorable(&corpus[i]) {
			continue
		}
		for _, t := range corpus[i].Tags {
			if t.Seeds {
				p.entries[fill[t.TagID]] = int32(i)
				fill[t.TagID]++
			}
		}
	}
	return p
}

// pairScan is the per-image scratch the walk reuses. stamp marks which
// candidates the current seed has already seen, so dedup costs one
// compare instead of sorting the gathered list.
type pairScan struct {
	seen    []int32
	partial []float64
	cand    []int32
	ordered []tags.SimilarityTag
	stamp   int32
}

func newPairScan(n int) *pairScan {
	return &pairScan{seen: make([]int32, n), partial: make([]float64, n)}
}

// scorePairsFrom scores image i against every higher-indexed image it
// could form an admissible pair with, feeding both sides' top-K
// lists. Each unordered pair is handled exactly once, from its
// lower-indexed member: the score is symmetric, so one computation
// serves both sides. The type partition keeps a manga match out of a
// still image's results and vice versa, matching the Similar-entries
// panel.
func scorePairsFrom(corpus []tags.SimilarityCorpusImage, postings *tagPostings, i int, threshold float64, matches [][]tagPairCandidate, scan *pairScan) {
	img := &corpus[i]
	if !scorable(img) {
		return
	}
	floor := sharedFloor(img, threshold)
	prefix, outside := prefixTags(img, floor, scan)
	if len(prefix) == 0 {
		return
	}
	// Shared weight cannot exceed either side's norm, so clearing the
	// threshold puts the candidate's norm inside a band around this
	// one's: below it the pair is capped by the candidate's own mass,
	// above it by this image's. The carrier row is already in cache from
	// the type check, which makes the band the cheapest rejection
	// available - and most of the library sits outside it.
	loNorm := threshold * threshold * img.Norm
	hiNorm := img.Norm / (threshold * threshold)
	scan.stamp++
	scan.cand = scan.cand[:0]
	for _, t := range prefix {
		for _, j := range postings.carriers(t.TagID) {
			other := &corpus[j]
			if j <= int32(i) || other.CBZ != img.CBZ {
				continue
			}
			if scan.seen[j] != scan.stamp {
				scan.seen[j] = scan.stamp
				scan.partial[j] = 0
				if other.Norm >= loNorm && other.Norm <= hiNorm {
					scan.cand = append(scan.cand, j)
				}
			}
			scan.partial[j] += t.Weight
		}
	}
	for _, j := range scan.cand {
		other := &corpus[j]
		// The prefix already contributed everything it can; the tags it
		// left behind can add at most their own mass. Measured against
		// what this candidate actually has to reach - not the band's
		// floor, which every carrier of a heavy prefix tag clears - that
		// settles most candidates without touching either tag list.
		if scan.partial[j]+outside < threshold*math.Sqrt(img.Norm*other.Norm) {
			continue
		}
		shared, n := sharedWeight(img.Tags, other.Tags)
		if n < tagPairMinShared {
			continue
		}
		score := tags.SimilarityScore(shared, img.Norm, other.Norm)
		if score < threshold {
			continue
		}
		matches[i] = insertTopK(matches[i], tagPairCandidate{imageID: other.ID, score: score})
		matches[j] = insertTopK(matches[j], tagPairCandidate{imageID: img.ID, score: score})
	}
}

// sharedFloor is the least shared weight any admissible pair seeded at
// img can carry. The other side's norm is at least the shared weight
// itself - a shared tag is counted on both sides - so clearing the
// threshold forces shared >= threshold^2 * norm.
func sharedFloor(img *tags.SimilarityCorpusImage, threshold float64) float64 {
	return threshold * threshold * img.Norm
}

// prefixTags returns the heaviest seeding tags whose removal would
// leave less than floor of weight, plus the mass left outside them.
// Every admissible pair shares at least one prefix tag, so only their
// postings need walking for candidates - and the tags this cuts are
// exactly the popular, low-weight ones whose postings dominate the
// scan. Tags too popular to seed keep their mass in the running total:
// they can still be shared, so the walk has to assume they are.
func prefixTags(img *tags.SimilarityCorpusImage, floor float64, scan *pairScan) (prefix []tags.SimilarityTag, outside float64) {
	scan.ordered = append(scan.ordered[:0], img.Tags...)
	ordered := scan.ordered
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].Weight > ordered[b].Weight })
	remaining := img.Norm
	prefix = ordered[:0]
	for _, t := range ordered {
		if remaining < floor {
			break
		}
		if !t.Seeds {
			continue
		}
		prefix = append(prefix, t)
		remaining -= t.Weight
	}
	return prefix, remaining
}

// sharedWeight sums the weights of the tags both id-sorted lists carry,
// and reports how many of them say something about the subject. A tag
// too popular to seed a scan is too popular to count as evidence
// either - both images being tagged "1girl" is not something the pair
// has in common - so it adds weight but not count.
func sharedWeight(a, b []tags.SimilarityTag) (float64, int) {
	var sum float64
	n := 0
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].TagID < b[j].TagID:
			i++
		case a[i].TagID > b[j].TagID:
			j++
		default:
			sum += a[i].Weight
			if a[i].Seeds {
				n++
			}
			i++
			j++
		}
	}
	return sum, n
}

// insertTopK keeps the best tagPairTopK candidates ordered by score
// descending, image id ascending.
func insertTopK(list []tagPairCandidate, c tagPairCandidate) []tagPairCandidate {
	pos := len(list)
	for pos > 0 {
		prev := list[pos-1]
		if prev.score > c.score || (prev.score == c.score && prev.imageID < c.imageID) {
			break
		}
		pos--
	}
	if pos >= tagPairTopK {
		return list
	}
	list = append(list, tagPairCandidate{})
	copy(list[pos+1:], list[pos:])
	list[pos] = c
	if len(list) > tagPairTopK {
		list = list[:tagPairTopK]
	}
	return list
}

// tagPairInsert is one admitted pair, canonicalised and ready to file.
type tagPairInsert struct {
	lo, hi int64
	score  float64
}

// admitTagPair runs the two read probes that gate the queue: a pair
// already carrying a declared relation or a not-related mark stays out,
// and so does one whose images share a collection that opted out of
// relation finding.
func admitTagPair(ctx context.Context, database *db.DB, seedID int64, c tagPairCandidate) (tagPairInsert, bool, error) {
	lo, hi := canonicalPair(seedID, c.imageID)
	related, err := pairHasDeclaredRelation(ctx, database, lo, hi)
	if err != nil || related {
		return tagPairInsert{}, false, err
	}
	hidden, err := pairSharesPrivateCollection(ctx, database, lo, hi)
	if err != nil || hidden {
		return tagPairInsert{}, false, err
	}
	return tagPairInsert{lo: lo, hi: hi, score: c.score}, true, nil
}

// storeTagPairTx files one admitted pair. A pair already queued by the
// phash walk is upgraded in place to record that both detectors agree,
// which is a stronger prior than either alone. Reports whether a new row
// landed.
func storeTagPairTx(tx *sql.Tx, p tagPairInsert) (bool, error) {
	res, err := tx.Exec(
		`UPDATE potential_relation_pairs SET source = ?, score = ?
		  WHERE a_image_id = ? AND b_image_id = ? AND source = ?`,
		SourceBoth, p.score, p.lo, p.hi, SourcePhash)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	res, err = tx.Exec(
		`INSERT OR IGNORE INTO potential_relation_pairs
		     (a_image_id, b_image_id, distance, created_at, source, score)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.lo, p.hi, TagPairDistance(p.score), time.Now().UTC().Format(time.RFC3339), SourceTags, p.score)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// A re-walk after tag edits can find a stronger score for an
	// already-queued pair. The row is the pair's best evidence, so
	// raise it rather than keeping the older value. Scoped to
	// tag-seeded rows: a `both` row's distance is the phash walk's
	// real hamming distance, not derived from the score.
	_, err = tx.Exec(
		`UPDATE potential_relation_pairs SET score = ?, distance = ?
		  WHERE a_image_id = ? AND b_image_id = ? AND source = ? AND score < ?`,
		p.score, TagPairDistance(p.score), p.lo, p.hi, SourceTags, p.score)
	return false, err
}

// pairSharesPrivateCollection reports whether both images sit in a
// collection that has not opted into relation finding. The session
// hides such pairs when it walks the queue; dropping them here keeps
// the table from filling with rows no session will ever show, which
// tag similarity would otherwise do constantly - the pages of one
// collection share nearly every tag.
func pairSharesPrivateCollection(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	var excluded int
	// The clause names the b-side first, so the binds follow that order
	// rather than the pair's.
	err := database.Read.QueryRowContext(ctx,
		`SELECT `+collectionPairExclusion("?", "?"), b, a).Scan(&excluded)
	if err != nil {
		return false, err
	}
	return excluded == 0, nil
}

// collectionPairExclusion returns the clause that hides a pair whose
// two images share a collection which has not opted into relation
// finding - membership already relates them. Queue rows carry the same
// verdict as the stored collection_hidden flag (db.Bootstrap's
// pairHiddenProbe triggers); this clause serves the admission probe,
// which runs before a row exists to stamp.
func collectionPairExclusion(aCol, bCol string) string {
	return `NOT EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = ` + bCol + `
		WHERE ca.image_id = ` + aCol + `
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))`
}

// pairHasDeclaredRelation reports whether the pair already carries a
// relation or a not-related mark. Unlike pairAlreadyKnown it ignores
// the queue, so a caller can tell "already decided" from "already
// queued" and upgrade the latter instead of skipping it.
func pairHasDeclaredRelation(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	tx, err := database.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	return pairHasOtherRelationTx(tx, a, b, "")
}
