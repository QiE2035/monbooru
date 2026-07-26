package relations

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
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

// tagPairMinTags is the floor both sides must clear for admission. Two
// three-tag images sharing all three score a perfect 1.0 on almost no
// evidence; a search can afford that noise because the operator is
// looking, a work queue cannot.
const tagPairMinTags = 4

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
	corpus, err := tags.LoadSimilarityCorpus(database, tagPairMinTags)
	if err != nil {
		return 0, fmt.Errorf("load tag-pair corpus: %w", err)
	}
	postings := make(map[int64][]int32)
	for i, img := range corpus {
		for _, t := range img.Tags {
			postings[t.TagID] = append(postings[t.TagID], int32(i))
		}
	}
	matches := make([][]tagPairCandidate, len(corpus))
	for i := range corpus {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if progress != nil && i%64 == 0 {
			progress(i, len(corpus), "tag probing")
		}
		scorePairsFrom(corpus, postings, i, threshold, matches)
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

// scorePairsFrom scores image i against every higher-indexed image it
// could form an admissible pair with, feeding both sides' top-K
// lists. Each unordered pair is handled exactly once, from its
// lower-indexed member: the shared weight is the same either way, and
// both directions' scores come from it. The type partition keeps a
// manga match out of a still image's results and vice versa, matching
// the Similar-entries panel.
func scorePairsFrom(corpus []tags.SimilarityCorpusImage, postings map[int64][]int32, i int, threshold float64, matches [][]tagPairCandidate) {
	img := &corpus[i]
	prefix := prefixTags(img, sharedFloor(img, threshold))
	if len(prefix) == 0 {
		return
	}
	var cand []int32
	for _, t := range prefix {
		for _, j := range postings[t.TagID] {
			if j > int32(i) && corpus[j].CBZ == img.CBZ {
				cand = append(cand, j)
			}
		}
	}
	if len(cand) == 0 {
		return
	}
	slices.Sort(cand)
	cand = slices.Compact(cand)
	seedable := len(img.Tags) >= tagPairMinTags
	for _, j := range cand {
		other := &corpus[j]
		shared := sharedWeight(img.Tags, other.Tags)
		if seedable {
			if score := tags.SimilarityScore(shared, img.Norm, other.TagCount); score >= threshold {
				matches[i] = insertTopK(matches[i], tagPairCandidate{imageID: other.ID, score: score})
			}
		}
		if len(other.Tags) >= tagPairMinTags {
			if score := tags.SimilarityScore(shared, other.Norm, img.TagCount); score >= threshold {
				matches[j] = insertTopK(matches[j], tagPairCandidate{imageID: img.ID, score: score})
			}
		}
	}
}

// sharedFloor is the least shared weight any admissible pair seeded
// at img can carry. Outbound, the candidate holds at least
// tagPairMinTags rows, so the denominator is at least
// sqrt(norm * minTags). Inbound, the other side's norm is at least
// the shared weight itself - a shared tag is counted on both sides -
// so clearing the threshold forces shared >= threshold^2 * tag_count.
func sharedFloor(img *tags.SimilarityCorpusImage, threshold float64) float64 {
	in := threshold * threshold * float64(img.TagCount)
	if len(img.Tags) < tagPairMinTags {
		return in
	}
	out := threshold * math.Sqrt(img.Norm*float64(tagPairMinTags))
	return math.Min(out, in)
}

// prefixTags returns the heaviest tags whose removal would leave less
// than floor of weight. Every admissible pair shares at least one of
// them, so only their postings need walking for candidates - and the
// tags this cuts are exactly the popular, low-weight ones whose
// postings dominate the scan.
func prefixTags(img *tags.SimilarityCorpusImage, floor float64) []tags.SimilarityTag {
	ordered := slices.Clone(img.Tags)
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].Weight > ordered[b].Weight })
	remaining := img.Norm
	for k := range ordered {
		if remaining < floor {
			return ordered[:k]
		}
		remaining -= ordered[k].Weight
	}
	return ordered
}

// sharedWeight sums the weights of the tags both id-sorted lists
// carry.
func sharedWeight(a, b []tags.SimilarityTag) float64 {
	var sum float64
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].TagID < b[j].TagID:
			i++
		case a[i].TagID > b[j].TagID:
			j++
		default:
			sum += a[i].Weight
			i++
			j++
		}
	}
	return sum
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
	// A pair admissible both ways is scored once per direction, and the
	// two scores generally differ. The row is the pair's best evidence,
	// so raise it rather than keeping whichever direction inserted
	// first. Scoped to tag-seeded rows: a `both` row's distance is the
	// phash walk's real hamming distance, not derived from the score.
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
