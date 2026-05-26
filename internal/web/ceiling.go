package web

import (
	"net/http"
	"strings"
	"sync"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tags"
)

// ratingCeilingCookieName is the single point of truth for the cookie
// name. The handler at POST /internal/rating-ceiling writes it; the
// resolver below reads it; nowhere else should reference the literal.
const ratingCeilingCookieName = "monbooru_rating_ceiling"

// Ceiling carries the resolved rating-ceiling state for one request.
// Construct via resolveCeiling so callers share the per-request cache of
// excluded tag ids and the optional tainted-image set.
//
// A Ceiling is safe to share across goroutines for one request. The
// sidebar handlers fan out to 6 worker goroutines that all consult the
// same *Ceiling; the mutex below guards the lazy caches against the
// resulting concurrent first-access. The hot path (cache hit) reads
// the cached slice / map directly under a single sync.Mutex acquire.
type Ceiling struct {
	level string
	cx    *galleryCtx

	mu             sync.Mutex
	excludedIDs    []int64
	excludedLoaded bool
	tainted        map[int64]bool
	taintedLoaded  bool
	taintedErr     error
}

// resolveCeiling reads the cookie and returns a Ceiling bound to cx.
// cx may be nil (no active gallery) - the resolver still works for AST
// shapes that don't need the tag-id resolution. ExcludedTagIDs and
// TaintedImageIDs return nil when cx is nil.
func resolveCeiling(r *http.Request, cx *galleryCtx) *Ceiling {
	return &Ceiling{level: readRatingCookie(r), cx: cx}
}

// readRatingCookie parses the cookie value. Empty string and "explicit"
// both mean "no ceiling"; anything outside the closed enum is dropped to
// "" so a stale or hand-crafted cookie can't inject arbitrary AST values.
func readRatingCookie(r *http.Request) string {
	c, err := r.Cookie(ratingCeilingCookieName)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "general", "sensitive", "questionable", "explicit":
		return c.Value
	}
	return ""
}

// writeRatingCookie sets or clears the cookie. level=explicit (or any
// out-of-enum value) clears it so the empty-storage steady state means
// "no ceiling".
func writeRatingCookie(w http.ResponseWriter, level string) {
	switch level {
	case "general", "sensitive", "questionable":
		http.SetCookie(w, &http.Cookie{
			Name:     ratingCeilingCookieName,
			Value:    level,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
	default:
		http.SetCookie(w, &http.Cookie{
			Name:   ratingCeilingCookieName,
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
}

// Level returns the raw cookie value. Empty string and "explicit" both
// mean "no ceiling"; callers that care about that distinction should use
// IsActive instead.
func (c *Ceiling) Level() string {
	if c == nil {
		return ""
	}
	return c.level
}

// IsActive reports whether a ceiling will actually filter anything. The
// no-cookie state and explicit-cookie state are both inactive: an
// explicit cookie is a no-op in the same way the empty cookie is, since
// the rating vocabulary tops out at explicit.
func (c *Ceiling) IsActive() bool {
	if c == nil {
		return false
	}
	return c.level != "" && c.level != "explicit"
}

// Apply AND-chains a NotExpr per rating level above the ceiling onto
// userExpr. The emitted AST shape is the contract fastCountCeiling
// recognises - keep this function as the sole producer. An empty or
// "explicit" ceiling returns userExpr unchanged.
func (c *Ceiling) Apply(userExpr search.Expr) search.Expr {
	if c == nil || !c.IsActive() {
		return userExpr
	}
	rank := -1
	for i, l := range tags.RatingLevels {
		if l == c.level {
			rank = i
			break
		}
	}
	if rank < 0 || rank >= len(tags.RatingLevels)-1 {
		return userExpr
	}
	var ce search.Expr
	for i := rank + 1; i < len(tags.RatingLevels); i++ {
		not := search.NotExpr{Expr: search.FilterExpr{Key: "rating", Val: tags.RatingLevels[i]}}
		if ce == nil {
			ce = not
		} else {
			ce = search.AndExpr{Left: ce, Right: not}
		}
	}
	if userExpr == nil {
		return ce
	}
	return search.AndExpr{Left: userExpr, Right: ce}
}

// ExcludedTagIDs returns the tag ids whose rating rank is strictly above
// the ceiling. Memoised per Ceiling so multiple call sites in one
// request pay the SELECT once. Returns nil when the ceiling is inactive
// or the active gallery isn't available.
func (c *Ceiling) ExcludedTagIDs() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.excludedLoaded {
		return c.excludedIDs
	}
	if !c.IsActive() {
		return nil
	}
	c.excludedLoaded = true
	if c.cx == nil || c.cx.TagSvc == nil {
		return nil
	}
	c.excludedIDs = c.cx.TagSvc.RatingTagIDsAbove(c.level)
	return c.excludedIDs
}

// WhereOne returns a NOT EXISTS predicate gating col on the absence of
// any rating tag above the ceiling. Returns ("", nil) when the ceiling
// is inactive so the caller can omit the WHERE entirely and keep the
// covering scan.
func (c *Ceiling) WhereOne(col string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	in := strings.Join(placeholders, ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return `NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + col + ` AND it.tag_id IN (` + in + `))`, args
}

// WhereTwo returns a pair of NOT EXISTS predicates ANDed together that
// gate each side of a paired query on the absence of any rating tag
// above the ceiling. Returns ("", nil) when the ceiling is inactive.
func (c *Ceiling) WhereTwo(leftCol, rightCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	in := strings.Join(placeholders, ",")
	tmpl := func(col string) string {
		return `NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + col + ` AND it.tag_id IN (` + in + `))`
	}
	args := make([]any, 0, 2*len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	return tmpl(leftCol) + " AND " + tmpl(rightCol), args
}

// WhereGroupClean returns a NOT EXISTS predicate that drops a group
// row when any of its members carries a rating above the ceiling. The
// predicate is meant to be ANDed onto a `... FROM <groups_table> g`
// scan: callers wire `groupCol` to the group-id column (e.g.
// `dup_groups.id`) and `membersTable` to the per-member join table
// (`dup_group_members`).
//
// Returns ("", nil) when the ceiling is inactive so the caller can
// skip the predicate entirely and keep the covering scan.
func (c *Ceiling) WhereGroupClean(membersTable, groupCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	return `NOT EXISTS (
		SELECT 1 FROM ` + membersTable + ` mr
		JOIN image_tags it ON it.image_id = mr.image_id
		WHERE mr.group_id = ` + groupCol + ` AND it.tag_id IN (` + strings.Join(placeholders, ",") + `)
	)`, args
}

// TaintedImageIDs returns the set of image ids whose tag list intersects
// the excluded rating ids. Used to drop whole relation chains / trees
// when any member exceeds the ceiling. Lazy - the SELECT runs only on
// first call, then is cached for the rest of the request. Returns nil
// when the ceiling is inactive.
//
// The mutex is held only around the cache-slot read and the result
// commit - the SELECT runs unlocked so a sidebar fan-out's six
// goroutines don't serialise behind it on a cache miss. A race that
// runs the SELECT twice is harmless (last writer wins, both reads
// see the same DB state).
func (c *Ceiling) TaintedImageIDs() (map[int64]bool, error) {
	if c == nil || !c.IsActive() {
		return nil, nil
	}
	ids := c.ExcludedTagIDs()
	c.mu.Lock()
	if c.taintedLoaded {
		t, err := c.tainted, c.taintedErr
		c.mu.Unlock()
		return t, err
	}
	c.mu.Unlock()
	if len(ids) == 0 || c.cx == nil || c.cx.DB == nil {
		c.mu.Lock()
		c.taintedLoaded = true
		c.mu.Unlock()
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := c.cx.DB.Read.Query(
		`SELECT DISTINCT image_id FROM image_tags WHERE tag_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		c.mu.Lock()
		c.taintedLoaded = true
		c.taintedErr = err
		c.mu.Unlock()
		return nil, err
	}
	defer rows.Close()
	ids, scanErr := db.ScanIDs(rows)
	if scanErr != nil {
		c.mu.Lock()
		c.taintedLoaded = true
		c.taintedErr = scanErr
		c.mu.Unlock()
		return nil, scanErr
	}
	tainted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		tainted[id] = true
	}
	c.mu.Lock()
	c.tainted = tainted
	c.taintedLoaded = true
	c.mu.Unlock()
	return tainted, nil
}

// AnyTainted reports whether any id in ids is tainted under c. A nil
// map (inactive ceiling) makes the check a no-op so call sites can
// treat the helper as policy-aware without an extra IsActive guard.
func (c *Ceiling) AnyTainted(ids []int64) bool {
	tainted, _ := c.TaintedImageIDs()
	if tainted == nil {
		return false
	}
	for _, id := range ids {
		if tainted[id] {
			return true
		}
	}
	return false
}

