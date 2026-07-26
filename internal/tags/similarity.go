package tags

import (
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
)

// Tag-set similarity between two images: the metric behind the
// `similar:` search keyword.

// errVisibleCount surfaces a failed visible-count read, which leaves
// rarity undefined and so has no usable fallback.
var errVisibleCount = errors.New("tags: visible image count unavailable")

// similarMaxTagUsage bounds which of a seed's tags take part in a
// score. Rarity already tapers a popular tag's weight toward zero, but
// a tag sitting on a large share of the library would still drag every
// one of those rows into the candidate scan, so the cap stays.
const similarMaxTagUsage = relatedMaxTagUsage

// categoryWeights scales a tag's rarity by how strongly its namespace
// identifies the subject. Artist is the strongest signal; character
// and copyright are cluster-level and rarity already suppresses them
// among same-character pairs. Categories absent here weigh 1; meta
// never reaches the map because it is excluded from the counted set.
var categoryWeights = map[string]float64{
	"artist":    3.0,
	"character": 2.0,
	"copyright": 2.0,
}

// SimilarityTag is one counted seed tag and what a candidate earns for
// carrying it.
type SimilarityTag struct {
	TagID  int64
	Weight float64
}

// SimilaritySeed carries the scoring inputs for one seed image: its
// counted tags with their weights, and the seed-side norm the cosine
// divides by. An empty Tags slice means the seed has nothing to match
// on - untagged, meta-only, or every tag over the usage cap - and
// every score against it is zero.
type SimilaritySeed struct {
	ImageID int64
	Tags    []SimilarityTag
	Norm    float64
}

// TagIDs returns the counted tag ids.
func (s SimilaritySeed) TagIDs() []int64 {
	ids := make([]int64, len(s.Tags))
	for i, t := range s.Tags {
		ids[i] = t.TagID
	}
	return ids
}

// WeightCase renders the per-tag weight lookup a scoring sum reads,
// plus its bind args. Resolving weights here rather than joining tags
// keeps the aggregate to one index range per seed tag; anything
// outside the counted set weighs 0.
func (s SimilaritySeed) WeightCase() (string, []any) {
	var b strings.Builder
	b.Grow(len(s.Tags)*16 + 24)
	args := make([]any, 0, len(s.Tags)*2)
	b.WriteString("CASE it.tag_id")
	for _, t := range s.Tags {
		b.WriteString(" WHEN ? THEN ?")
		args = append(args, t.TagID, t.Weight)
	}
	b.WriteString(" ELSE 0 END")
	return b.String(), args
}

// Score returns the weighted-cosine score of a candidate carrying
// shared weight against a seed, saturating at 1. tagCount is the
// candidate's raw tag count, which is what the seed norm is compared
// against - the two sides sit on different scales and the threshold
// absorbs the offset.
func (s SimilaritySeed) Score(shared float64, tagCount int) float64 {
	return SimilarityScore(shared, s.Norm, tagCount)
}

// SimilarityScore is the one weighted-cosine formula every scoring
// path shares: shared weight over the geometric mean of the seed norm
// and the candidate's raw tag count, saturating at 1.
func SimilarityScore(shared, norm float64, tagCount int) float64 {
	if norm <= 0 || tagCount <= 0 {
		return 0
	}
	return math.Min(1, shared/math.Sqrt(norm*float64(tagCount)))
}

// LoadSimilaritySeed reads imageID's counted tags and weights each by
// rarity times its category multiplier. Rarity is ln(N / usage_count)
// against the visible image count, so a tag carried by (almost) every
// image weighs ~0 and drops out of both the numerator and the norm on
// its own.
func LoadSimilaritySeed(database *db.DB, imageID int64) (SimilaritySeed, error) {
	seed := SimilaritySeed{ImageID: imageID}
	n, ok := database.VisibleCount()
	if !ok {
		return seed, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return seed, nil
	}
	rows, err := database.Read.Query(
		`SELECT it.tag_id, t.usage_count, tc.name
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND tc.name != 'meta' AND t.usage_count <= ?
		  ORDER BY it.tag_id`,
		imageID, similarMaxTagUsage,
	)
	if err != nil {
		return seed, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tagID, usage int64
		var category string
		if err := rows.Scan(&tagID, &usage, &category); err != nil {
			return seed, err
		}
		w := tagWeight(visible, usage, category)
		if w <= 0 {
			continue
		}
		seed.Tags = append(seed.Tags, SimilarityTag{TagID: tagID, Weight: w})
		seed.Norm += w
	}
	return seed, rows.Err()
}

// SimilarityCorpusImage is one scorable image in a whole-library
// pass: the counted tags LoadSimilaritySeed would build for it,
// sorted by tag id, plus the raw tag count the candidate-side
// denominator reads and the cbz flag the type partition compares.
type SimilarityCorpusImage struct {
	ID       int64
	TagCount int
	CBZ      bool
	Tags     []SimilarityTag
	Norm     float64
}

// LoadSimilarityCorpus reads every visible image carrying at least
// minTagCount tags in one pass, ordered by image id. Images whose
// counted set comes back empty are dropped: with nothing to share
// they can neither seed a match nor be one.
func LoadSimilarityCorpus(database *db.DB, minTagCount int) ([]SimilarityCorpusImage, error) {
	n, ok := database.VisibleCount()
	if !ok {
		return nil, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return nil, nil
	}
	rows, err := database.Read.Query(
		`SELECT it.image_id, it.tag_id, t.usage_count, tc.name, i.tag_count, i.file_type
		   FROM image_tags it
		   JOIN images i ON i.id = it.image_id
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE i.is_missing = 0 AND i.tag_count >= ?
		    AND tc.name != 'meta' AND t.usage_count <= ?
		  ORDER BY it.image_id, it.tag_id`,
		minTagCount, similarMaxTagUsage,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	weights := make(map[int64]float64)
	var corpus []SimilarityCorpusImage
	for rows.Next() {
		var imageID, tagID, usage int64
		var category, fileType string
		var tagCount int
		if err := rows.Scan(&imageID, &tagID, &usage, &category, &tagCount, &fileType); err != nil {
			return nil, err
		}
		w, ok := weights[tagID]
		if !ok {
			w = tagWeight(visible, usage, category)
			weights[tagID] = w
		}
		if w <= 0 {
			continue
		}
		if len(corpus) == 0 || corpus[len(corpus)-1].ID != imageID {
			corpus = append(corpus, SimilarityCorpusImage{
				ID:       imageID,
				TagCount: tagCount,
				CBZ:      fileType == "cbz",
			})
		}
		img := &corpus[len(corpus)-1]
		img.Tags = append(img.Tags, SimilarityTag{TagID: tagID, Weight: w})
		img.Norm += w
	}
	return corpus, rows.Err()
}

// ScorePercentsAgainst returns each candidate's score against the
// seed as a whole percent, keyed by image id and omitting anything
// that shares nothing. Scoped to the ids a page already holds, so the
// aggregate stays bounded by the page size rather than the library.
func ScorePercentsAgainst(database *db.DB, seedID int64, ids []int64) (map[int64]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seed, err := LoadSimilaritySeed(database, seedID)
	if err != nil || len(seed.Tags) == 0 {
		return nil, err
	}
	tagPlaceholders, tagArgs := db.InPlaceholders(seed.TagIDs())
	idPlaceholders, idArgs := db.InPlaceholders(ids)
	weightCase, weightArgs := seed.WeightCase()
	// Bind order follows the placeholders' order in the statement text,
	// which puts the scoring sum's weights ahead of the WHERE.
	args := make([]any, 0, len(weightArgs)+len(idArgs)+len(tagArgs))
	args = append(args, weightArgs...)
	args = append(args, idArgs...)
	args = append(args, tagArgs...)
	rows, err := database.Read.Query(
		`SELECT it.image_id, sum(`+weightCase+`) AS shared, im.tag_count
		   FROM image_tags it
		   JOIN images im ON im.id = it.image_id
		  WHERE it.image_id IN (`+idPlaceholders+`)
		    AND it.tag_id IN (`+tagPlaceholders+`)
		  GROUP BY it.image_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int, len(ids))
	for rows.Next() {
		var id int64
		var shared float64
		var tagCount int
		if err := rows.Scan(&id, &shared, &tagCount); err != nil {
			return nil, err
		}
		if pct := int(math.Round(seed.Score(shared, tagCount) * 100)); pct > 0 {
			out[id] = pct
		}
	}
	return out, rows.Err()
}

// SharedTag is one tag both images carry and the weight it contributed
// to their score.
type SharedTag struct {
	Name   string
	Weight float64
}

// SharedTags returns what two images have in common, heaviest first,
// capped at limit, plus the total number of shared counted tags. This
// is the evidence behind a tag-similarity score: a pair can share
// forty tags and look nothing alike, so which tags drove the match is
// what makes the score judgeable.
func SharedTags(database *db.DB, a, b int64, limit int) ([]SharedTag, int, error) {
	seed, err := LoadSimilaritySeed(database, a)
	if err != nil || len(seed.Tags) == 0 {
		return nil, 0, err
	}
	weights := make(map[int64]float64, len(seed.Tags))
	for _, t := range seed.Tags {
		weights[t.TagID] = t.Weight
	}
	placeholders, args := db.InPlaceholders(seed.TagIDs())
	rows, err := database.Read.Query(
		`SELECT it.tag_id, t.name
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		  WHERE it.image_id = ? AND it.tag_id IN (`+placeholders+`)`,
		append([]any{b}, args...)...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var shared []SharedTag
	for rows.Next() {
		var tagID int64
		var name string
		if err := rows.Scan(&tagID, &name); err != nil {
			return nil, 0, err
		}
		shared = append(shared, SharedTag{Name: name, Weight: weights[tagID]})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].Weight != shared[j].Weight {
			return shared[i].Weight > shared[j].Weight
		}
		return shared[i].Name < shared[j].Name
	})
	total := len(shared)
	if limit > 0 && total > limit {
		shared = shared[:limit]
	}
	return shared, total, nil
}

// tagWeight is one tag's contribution. A tag on at least as many
// images as the library holds visible says nothing about the subject,
// so it weighs nothing.
func tagWeight(visible, usage int64, category string) float64 {
	if usage <= 0 || usage >= visible {
		return 0
	}
	w := math.Log(float64(visible) / float64(usage))
	if m, ok := categoryWeights[category]; ok {
		w *= m
	}
	return w
}
