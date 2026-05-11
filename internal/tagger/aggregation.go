package tagger

import (
	"math"
	"sort"
)

// DefaultPerCategoryTopK is the fallback per-category emission cap used
// when a tagger has no PerCategoryTopK override (and the catalog entry
// has no DefaultTopK either). The numbers are tuned for the WD14 /
// JoyTag / Camie label distributions: rare-attribution categories sit
// low so a noisy run can't pile dozens of characters or artists onto
// one row; general is generous because it carries most of the
// descriptive surface. The "rating" cap is 1 so the per-image rating
// store ends up with exactly one tag - taggers that emit several
// rating signals on borderline content settle deterministically on
// the top-scoring one instead of needing the store step to pick.
var DefaultPerCategoryTopK = map[string]int{
	"character": 8,
	"copyright": 4,
	"artist":    4,
	"general":   25,
	"rating":    1,
}

// DefaultTopKFallback is applied to any category not listed in
// DefaultPerCategoryTopK. Custom operator-created categories land here.
const DefaultTopKFallback = 10

// ResolveTopK returns the active cap for a category given a tagger's
// PerCategoryTopK overrides. An explicit zero in the override map
// disables the cap (returns 0 = uncapped). A missing key falls through
// to DefaultPerCategoryTopK and then DefaultTopKFallback.
func ResolveTopK(overrides map[string]int, cat string) int {
	if overrides != nil {
		if v, ok := overrides[cat]; ok {
			return v
		}
	}
	if v, ok := DefaultPerCategoryTopK[cat]; ok {
		return v
	}
	return DefaultTopKFallback
}

// catNameByID reverses a name→id map for the top-K lookup. The map
// is small (tag_categories rarely exceeds a couple dozen rows) so a
// linear scan beats maintaining a parallel inverse map. Returns ""
// when the id is unknown; the caller then falls through to the
// fallback cap.
func catNameByID(catIDs map[string]int64, id int64) string {
	for name, cid := range catIDs {
		if cid == id {
			return name
		}
	}
	return ""
}

// ResolveMinHits returns the minimum number of pages/frames a label
// must score above the pre-floor on for it to survive the merge. Given
// the global min_hit_fraction and the frame count for this row:
//
//   - frameCount <= 1 (static image): always 1, so the aggregation
//     behaves identically to the legacy max-only path.
//   - fraction <= 0: 1, the operator opt-out.
//   - otherwise: clamp(ceil(fraction * frameCount), 2, 10).
//
// The lower bound of 2 is what makes the gate useful - a single flicker
// of a noisy label across a 500-page archive shouldn't be enough to
// imprint it on the row. The upper bound of 10 is a guardrail so a
// sparse-but-real label on a long manga still has a path through.
func ResolveMinHits(fraction float64, frameCount int) int {
	if frameCount <= 1 {
		return 1
	}
	if fraction <= 0 {
		return 1
	}
	raw := int(math.Ceil(fraction * float64(frameCount)))
	if raw < 2 {
		return 2
	}
	if raw > 10 {
		return 10
	}
	return raw
}

// CandidateLabel is the per-label metadata the aggregator needs after
// the caller has already resolved category routing (dispatch rules,
// single_general lifts, rating skips). One entry per label index from
// the model's tag vocabulary; Placeholder=true rows are dropped on
// sight.
type CandidateLabel struct {
	Name        string
	CatID       int64
	CatName     string
	Placeholder bool
}

// AggregatedCandidate is one (name, catID, mean-score) result that
// survives both the frequency gate and the per-category top-K cap.
// Ordering of the returned slice is undefined; callers that care
// (multi-tagger merge) re-key by tagKey.
type AggregatedCandidate struct {
	Name  string
	CatID int64
	Score float32
}

// AggregateOpts collects the per-merge knobs the pure aggregator
// needs. GlobalThreshold and CategoryThresholds gate which labels
// survive; PerCategoryTopK caps how many of the survivors land per
// category. MinHits is the resolved frame-count gate from
// ResolveMinHits.
type AggregateOpts struct {
	MinHits            int
	GlobalThreshold    float32
	CategoryThresholds map[string]float64
	PerCategoryTopK    map[string]int
}

// AggregateInferenceScores applies the §2 merge to per-frame label
// scores. perFrame[fIdx][labelIdx] is the label's score on that frame
// (zero / missing = no hit). labels[labelIdx] gives the post-routing
// metadata. The function:
//
//  1. drops near-zero scores (pre-floor 0.001).
//  2. tracks sum + hit count per label across every frame.
//  3. computes mean = sum / hits and gates on the per-category
//     threshold and the MinHits floor.
//  4. groups survivors by category, sorts by mean desc (name asc as
//     tiebreaker) and keeps at most ResolveTopK(opts.PerCategoryTopK,
//     CatName) per category.
//
// Returns the survivors in no particular order.
func AggregateInferenceScores(perFrame [][]float32, labels []CandidateLabel, opts AggregateOpts) []AggregatedCandidate {
	type accum struct {
		sum  float32
		hits int
	}
	agg := map[int]accum{}
	for _, scores := range perFrame {
		for idx, s := range scores {
			if s < 0.001 {
				continue
			}
			e := agg[idx]
			e.sum += s
			e.hits++
			agg[idx] = e
		}
	}

	byCat := map[int64][]AggregatedCandidate{}
	for idx, e := range agg {
		if idx >= len(labels) {
			continue
		}
		lbl := labels[idx]
		if lbl.Placeholder {
			continue
		}
		if e.hits < opts.MinHits {
			continue
		}
		mean := e.sum / float32(e.hits)
		threshold := opts.GlobalThreshold
		if v, ok := opts.CategoryThresholds[lbl.CatName]; ok {
			threshold = float32(v)
		}
		if mean < threshold {
			continue
		}
		byCat[lbl.CatID] = append(byCat[lbl.CatID],
			AggregatedCandidate{Name: lbl.Name, CatID: lbl.CatID, Score: mean})
	}

	var out []AggregatedCandidate
	for _, list := range byCat {
		// Tie-break by name asc so two equivalent runs produce the
		// same emission set. catName is the same for everyone in the
		// list so picking from list[0] is safe.
		catName := ""
		if len(list) > 0 {
			catName = catNameFromLabels(labels, list[0].CatID)
		}
		k := ResolveTopK(opts.PerCategoryTopK, catName)
		sort.Slice(list, func(i, j int) bool {
			if list[i].Score != list[j].Score {
				return list[i].Score > list[j].Score
			}
			return list[i].Name < list[j].Name
		})
		if k > 0 && len(list) > k {
			list = list[:k]
		}
		out = append(out, list...)
	}
	return out
}

// catNameFromLabels picks the CatName for the first label matching
// catID. The caller passes the same labels slice we already iterated;
// a linear scan is fine since the vocabulary is typically a couple
// thousand entries and this runs once per surviving category.
func catNameFromLabels(labels []CandidateLabel, catID int64) string {
	for i := range labels {
		if labels[i].CatID == catID {
			return labels[i].CatName
		}
	}
	return ""
}
