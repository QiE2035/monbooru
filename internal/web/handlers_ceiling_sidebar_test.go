package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSidebar_CeilingAwareFavoritedCounts pins the Favorites row on
// /internal/sidebar - both Favorites and Not-favorites should respect
// the ceiling so the totals add up to the ceiling-aware visible count.
func TestSidebar_CeilingAwareFavoritedCounts(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	safe, explicit := seedRatedPair(t, srv)
	// Mark both as favorites; under ceiling=sensitive only safe stays.
	for _, id := range []int64{safe, explicit} {
		if _, err := srv.db().Write.Exec(`UPDATE images SET is_favorited = 1 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	cx.InvalidateCaches()

	req := httptest.NewRequest("GET", "/internal/sidebar?ids=", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sidebar expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Favorites (1)") {
		t.Errorf("expected Favorites (1) under ceiling=sensitive; body slice: %s", favSlice(body))
	}
	if !strings.Contains(body, "Not favorites (0)") {
		t.Errorf("expected Not favorites (0) (no non-favourite visible rows); body slice: %s", favSlice(body))
	}
}

// TestRelationsHub_PhashMissingHonoursCeiling: two images, both
// phash NULL'd manually after ingest, one explicit-rated. The hub's
// PhashMissing count must drop from 2 to 1 when the operator's
// ceiling hides the explicit row. The template renders
// <strong>{{.Counts.PhashMissing}}</strong> image(s) without a
// phash, gated on `gt 0`.
func TestRelationsHub_PhashMissingHonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv)
	// gallery.Ingest computes a phash on a successful thumbnail. Null
	// it out so PhashMissing actually counts both rows.
	if _, err := srv.db().Write.Exec(`UPDATE images SET phash = NULL`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/relations", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/relations expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<strong>2</strong>") || !strings.Contains(body, "without a phash") {
		t.Errorf("no-ceiling: expected 2 image(s) without a phash; body slice: %s", phashMissingSlice(body))
	}

	req = httptest.NewRequest("GET", "/relations", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/relations expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, "<strong>1</strong>") || !strings.Contains(body, "without a phash") {
		t.Errorf("ceiling=sensitive: expected 1 image without a phash; body slice: %s", phashMissingSlice(body))
	}
}

func favSlice(body string) string {
	return sliceAround(body, "favorites-filter-section", 400)
}

func phashMissingSlice(body string) string {
	return sliceAround(body, "without a phash", 200)
}

func sliceAround(body, needle string, span int) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return "(needle not found: " + needle + ")"
	}
	start := idx - 100
	if start < 0 {
		start = 0
	}
	end := idx + span
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}

// TestGallery_HiddenByCeilingIndicator pins the footer "N hidden
// images in the current search" cell: under an active ceiling the
// matches count stays bare on the gallery status bar and the delta
// to the no-ceiling total surfaces inside the footer's
// `#footer-hidden` span. With no ceiling, the cell renders empty.
func TestGallery_HiddenByCeilingIndicator(t *testing.T) {
	srv := newTestServer(t)
	seedRatedPair(t, srv) // safe (general) + explicit

	// No ceiling: indicator off; matches line and footer cell are bare.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/ expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span id="footer-hidden" class="footer-hidden"></span>`) {
		t.Errorf("no-ceiling: footer cell should be empty; got: %s", footerHiddenSlice(body))
	}
	if !strings.Contains(body, `class="result-count">2</span>`) {
		t.Errorf("no-ceiling: expected 2 matches; got: %s", statusSlice(body))
	}

	// ceiling=sensitive: matches drops to 1, footer cell carries the
	// "1 hidden images in the current search" label.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body = w.Body.String()
	if !strings.Contains(body, `class="result-count">1</span>`) {
		t.Errorf("ceiling=sensitive: expected 1 match; got: %s", statusSlice(body))
	}
	if !strings.Contains(body, `id="footer-hidden"`) || !strings.Contains(body, "1 hidden images in the current search") {
		t.Errorf("ceiling=sensitive: expected footer hidden indicator with 1; got: %s", footerHiddenSlice(body))
	}
}

// TestGallery_HiddenByCeilingIndicator_FilteredQuery covers the
// branch that runs a second COUNT against the bare user expr. Two
// images carrying `rating:general` and `rating:explicit` plus the
// same shared tag - searching that tag under ceiling=sensitive must
// surface "1 match · 1 hidden".
func TestGallery_HiddenByCeilingIndicator_FilteredQuery(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	safe, explicit := seedRatedPair(t, srv)
	// Add a shared tag so a filtered search matches both rows.
	var generalCat int64
	srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalCat)
	tag, err := cx.TagSvc.GetOrCreateTag("shared", generalCat)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(safe, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(explicit, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	cx.InvalidateCaches()

	req := httptest.NewRequest("GET", "/?q=shared", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/ expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="result-count">1</span>`) {
		t.Errorf("expected 1 match (general only); got: %s", statusSlice(body))
	}
	if !strings.Contains(body, `id="footer-hidden"`) || !strings.Contains(body, "1 hidden images in the current search") {
		t.Errorf("expected 1 hidden footer indicator; got: %s", footerHiddenSlice(body))
	}
}

func statusSlice(body string) string {
	return sliceAround(body, `id="gallery-status"`, 300)
}

func footerHiddenSlice(body string) string {
	return sliceAround(body, `id="footer-hidden"`, 300)
}

// fetchRelationsHubBody returns the rendered /relations body under the
// supplied ceiling cookie (empty string = no cookie).
func fetchRelationsHubBody(t *testing.T, srv *Server, ceiling string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/relations", nil)
	if ceiling != "" {
		req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: ceiling})
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/relations expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// fetchRelationsBrowseBody returns the rendered /relations/browse?kind
// body under the supplied ceiling cookie.
func fetchRelationsBrowseBody(t *testing.T, srv *Server, kind, ceiling string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/relations/browse?kind="+kind, nil)
	if ceiling != "" {
		req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: ceiling})
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/relations/browse?kind=%s expected 200, got %d", kind, w.Code)
	}
	return w.Body.String()
}

// TestRelationsHub_DeclaredCountersHonourCeiling: each declared
// relation kind seeds two records, one of which has an explicit-
// rated member. Under ceiling=sensitive the counter must drop from
// 2 to 1 across DupGroups, AltGroups, VersionChains,
// DerivativeTrees, and NotRelatedPairs.
func TestRelationsHub_DeclaredCountersHonourCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	rel := cx.RelationsSvc

	// Each seedImage produces a black RGBA of the supplied dimensions;
	// gallery.Ingest dedupes by sha256, so the dims must be unique per
	// row. Hand each call a monotonically increasing size so the pairs
	// below land on distinct image ids.
	dim := 7
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}
	tag := func(id int64, level string) {
		if err := cx.TagSvc.AddTagToImage(id, ratingTagIDWeb(t, cx.DB, level), false, nil); err != nil {
			t.Fatal(err)
		}
	}

	dupSafeA, dupSafeB := mkImg("dup_safe_a.png"), mkImg("dup_safe_b.png")
	dupTaintA, dupTaintB := mkImg("dup_taint_a.png"), mkImg("dup_taint_b.png")
	tag(dupTaintB, "explicit")
	if err := rel.AddDuplicate(dupSafeA, dupSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDuplicate(dupTaintA, dupTaintB); err != nil {
		t.Fatal(err)
	}

	altSafeA, altSafeB := mkImg("alt_safe_a.png"), mkImg("alt_safe_b.png")
	altTaintA, altTaintB := mkImg("alt_taint_a.png"), mkImg("alt_taint_b.png")
	tag(altTaintB, "explicit")
	if err := rel.AddAlternate(altSafeA, altSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddAlternate(altTaintA, altTaintB); err != nil {
		t.Fatal(err)
	}

	verSafeP, verSafeC := mkImg("ver_safe_p.png"), mkImg("ver_safe_c.png")
	verTaintP, verTaintC := mkImg("ver_taint_p.png"), mkImg("ver_taint_c.png")
	tag(verTaintC, "explicit")
	if err := rel.AddVersionEdge(verSafeP, verSafeC); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddVersionEdge(verTaintP, verTaintC); err != nil {
		t.Fatal(err)
	}

	derSafeS, derSafeD := mkImg("der_safe_s.png"), mkImg("der_safe_d.png")
	derTaintS, derTaintD := mkImg("der_taint_s.png"), mkImg("der_taint_d.png")
	tag(derTaintD, "explicit")
	if err := rel.AddDerivativeEdge(derSafeS, derSafeD); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDerivativeEdge(derTaintS, derTaintD); err != nil {
		t.Fatal(err)
	}

	nrSafeA, nrSafeB := mkImg("nr_safe_a.png"), mkImg("nr_safe_b.png")
	nrTaintA, nrTaintB := mkImg("nr_taint_a.png"), mkImg("nr_taint_b.png")
	tag(nrTaintB, "explicit")
	if err := rel.AddNotRelated(nrSafeA, nrSafeB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddNotRelated(nrTaintA, nrTaintB); err != nil {
		t.Fatal(err)
	}

	// No ceiling: every counter sees 2.
	body := fetchRelationsHubBody(t, srv, "")
	for _, want := range []string{
		`<strong>2</strong> same-image group`,
		`<strong>2</strong> variant group`,
		`<strong>2</strong> revision chain`,
		`<strong>2</strong> based-on tree`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no-ceiling: expected %q on /relations; slice: %s", want, sliceAround(body, "Browse relations", 600))
		}
	}

	// ceiling=sensitive: every counter drops to 1 (the tainted half).
	body = fetchRelationsHubBody(t, srv, "sensitive")
	for _, want := range []string{
		`<strong>1</strong> same-image group`,
		`<strong>1</strong> variant group`,
		`<strong>1</strong> revision chain`,
		`<strong>1</strong> based-on tree`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ceiling=sensitive: expected %q on /relations; slice: %s", want, sliceAround(body, "Browse relations", 600))
		}
	}

	// /relations/browse tabs read Counts as well - the not_related tab
	// label "(N)" must drop too. Check via the browse-tab anchor.
	browse := fetchRelationsBrowseBody(t, srv, "not_related", "sensitive")
	if !strings.Contains(browse, `Not related <span class="field-hint">(1)`) {
		t.Errorf("ceiling=sensitive: expected Not related (1) tab label; slice: %s", sliceAround(browse, "Not related", 200))
	}
	// And sanity-check the no-ceiling tab shows (2).
	browse = fetchRelationsBrowseBody(t, srv, "not_related", "")
	if !strings.Contains(browse, `Not related <span class="field-hint">(2)`) {
		t.Errorf("no-ceiling: expected Not related (2) tab label; slice: %s", sliceAround(browse, "Not related", 200))
	}
}

// TestRelationsHub_ChainTreeCountsMatchBrowseCards pins the relations
// counter semantics: the chain / tree counter must report the number
// of cards /relations/browse renders, not the underlying edge count.
// A 3-image chain a -> b -> c is one chain card (one counter increment),
// not two edges; tainting the middle member drops the whole chain off
// /relations/browse, so the counter must follow it to zero. The same
// shape holds for a derivative tree.
func TestRelationsHub_ChainTreeCountsMatchBrowseCards(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	rel := cx.RelationsSvc

	dim := 11
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}
	tag := func(id int64, level string) {
		if err := cx.TagSvc.AddTagToImage(id, ratingTagIDWeb(t, cx.DB, level), false, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Three-image chain a -> b -> c. Two edges, one chain card.
	chainA, chainB, chainC := mkImg("chain_a.png"), mkImg("chain_b.png"), mkImg("chain_c.png")
	if err := rel.AddVersionEdge(chainA, chainB); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddVersionEdge(chainB, chainC); err != nil {
		t.Fatal(err)
	}

	// Three-image tree src -> {d1, d2}. Two edges, one tree card.
	treeSrc, treeD1, treeD2 := mkImg("tree_src.png"), mkImg("tree_d1.png"), mkImg("tree_d2.png")
	if err := rel.AddDerivativeEdge(treeSrc, treeD1); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDerivativeEdge(treeSrc, treeD2); err != nil {
		t.Fatal(err)
	}

	// No ceiling: each counter is 1 (one chain, one tree), matching the
	// single card the browse list renders for each kind.
	body := fetchRelationsHubBody(t, srv, "")
	if !strings.Contains(body, `<strong>1</strong> revision chain`) {
		t.Errorf("no-ceiling: expected 1 revision chain; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	if !strings.Contains(body, `<strong>1</strong> based-on tree`) {
		t.Errorf("no-ceiling: expected 1 based-on tree; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	browse := fetchRelationsBrowseBody(t, srv, "version", "")
	if !strings.Contains(browse, `Revision history <span class="field-hint">(1)`) {
		t.Errorf("no-ceiling: expected Revision history (1) tab; slice: %s", sliceAround(browse, "Revision history", 200))
	}
	browse = fetchRelationsBrowseBody(t, srv, "derivative", "")
	if !strings.Contains(browse, `Based on <span class="field-hint">(1)`) {
		t.Errorf("no-ceiling: expected Based on (1) tab; slice: %s", sliceAround(browse, "Based on", 200))
	}

	// Taint the middle of each group. The chain card and the tree card
	// both vanish from /relations/browse under ceiling=sensitive, so the
	// counters must follow them to zero.
	tag(chainB, "explicit")
	tag(treeD1, "explicit")

	body = fetchRelationsHubBody(t, srv, "sensitive")
	if !strings.Contains(body, `<strong>0</strong> revision chain`) {
		t.Errorf("ceiling=sensitive: expected 0 revision chain; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	if !strings.Contains(body, `<strong>0</strong> based-on tree`) {
		t.Errorf("ceiling=sensitive: expected 0 based-on tree; slice: %s", sliceAround(body, "Browse relations", 400))
	}
	browse = fetchRelationsBrowseBody(t, srv, "version", "sensitive")
	if !strings.Contains(browse, `Revision history <span class="field-hint">(0)`) {
		t.Errorf("ceiling=sensitive: expected Revision history (0) tab; slice: %s", sliceAround(browse, "Revision history", 200))
	}
	browse = fetchRelationsBrowseBody(t, srv, "derivative", "sensitive")
	if !strings.Contains(browse, `Based on <span class="field-hint">(0)`) {
		t.Errorf("ceiling=sensitive: expected Based on (0) tab; slice: %s", sliceAround(browse, "Based on", 200))
	}
}
