package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubPTRGraph answers /api/v1/ptr/tags with the supplied per-name results
// and records every name it was asked about.
func stubPTRGraph(t *testing.T, results map[string]any, asked *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ptr/tags" {
			t.Errorf("ptr lookup hit %q, want /api/v1/ptr/tags", r.URL.Path)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad ptr/tags body: %v", err)
		}
		*asked = append(*asked, body.Tags...)
		out := map[string]any{}
		for _, name := range body.Tags {
			if res, ok := results[name]; ok {
				out[name] = res
			} else {
				out[name] = map[string]any{"known": false}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}))
}

func postPTRLookup(t *testing.T, srv *Server, path string, form url.Values) {
	t.Helper()
	form.Set("_csrf", srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%s: %d, %s", path, w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.jobs.IsRunning() {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("ptr lookup job never drained")
	}
}

// The single-tag sweep adds unknown alias names, leaves existing tags alone,
// declares the implications, and backfills them onto images already carrying
// the parent.
func TestPTRLookupTag_AppliesAliasesAndImplications(t *testing.T) {
	srv := newTestServer(t)
	imgID := seedImage(t, srv, "ptr1.png", 6, 6)
	generalID := srv.Active().GeneralCategoryID
	parent, err := srv.tagSvc().GetOrCreateTag("1girl", generalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tagSvc().AddTagsToOneImage(imgID, []int64{parent.ID}, ""); err != nil {
		t.Fatal(err)
	}
	// An independent tag squatting one of the PTR's alias spellings: the
	// sweep must not repoint or merge it.
	taken, err := srv.tagSvc().GetOrCreateTag("solo_girl", generalID)
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	stub := stubPTRGraph(t, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"1girls", "solo_girl"},
			"implications": []string{"girl"},
		},
	}, &asked)
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})

	var isAlias, canonical int64
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias, canonical_tag_id FROM tags WHERE name = '1girls'`).Scan(&isAlias, &canonical); err != nil {
		t.Fatalf("alias 1girls not created: %v", err)
	}
	if isAlias != 1 || canonical != parent.ID {
		t.Errorf("1girls = (alias=%d, canonical=%d), want alias of %d", isAlias, canonical, parent.ID)
	}
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias FROM tags WHERE id = ?`, taken.ID).Scan(&isAlias); err != nil {
		t.Fatal(err)
	}
	if isAlias != 0 {
		t.Error("the existing solo_girl tag must not be repointed into an alias")
	}
	var n int
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id
		 WHERE ti.parent_tag_id = ? AND t.name = 'girl'`, parent.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("implication 1girl -> girl edges = %d, want 1", n)
	}
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'girl' AND it.is_implied = 1`, imgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("implied girl rows on the image = %d, want 1 (fan-out missing)", n)
	}
}

// A PTR that becomes unavailable mid-sweep stops the job with its partial
// tally instead of failing it, matching the batch lookup's degrade.
func TestPTRLookupTag_UnavailableStopsWithoutFailing(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.Active().GeneralCategoryID
	tag, err := srv.tagSvc().GetOrCreateTag("1girl", generalID)
	if err != nil {
		t.Fatal(err)
	}

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"ptr_unavailable"}`))
	}))
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-lookup", url.Values{})

	state := srv.jobs.Get()
	if state.Error != "" {
		t.Fatalf("job failed with %q, want a completed stop", state.Error)
	}
	if !strings.Contains(state.Summary, "unavailable") {
		t.Errorf("summary = %q, want it to name the PTR unavailability", state.Summary)
	}
}

// PTR tokens can be category-qualified; splitCategoryTag must land them in the
// named category rather than collapsing them into general.
func TestPTRLookupTag_CategoryQualifiedTokens(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.Active().GeneralCategoryID
	parent, err := srv.tagSvc().GetOrCreateTag("1girl", generalID)
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	stub := stubPTRGraph(t, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"character:foo"},
			"implications": []string{"copyright:bar"},
		},
	}, &asked)
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})

	charID := lookupCategoryID(srv.db(), "character")
	var isAlias, canonical int64
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias, canonical_tag_id FROM tags WHERE name = 'foo' AND category_id = ?`, charID,
	).Scan(&isAlias, &canonical); err != nil {
		t.Fatalf("alias foo not created in the character category: %v", err)
	}
	if isAlias != 1 || canonical != parent.ID {
		t.Errorf("foo = (alias=%d, canonical=%d), want a character-category alias of %d", isAlias, canonical, parent.ID)
	}

	copyID := lookupCategoryID(srv.db(), "copyright")
	var n int
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications ti JOIN tags t ON t.id = ti.implied_tag_id
		 WHERE ti.parent_tag_id = ? AND t.name = 'bar' AND t.category_id = ?`, parent.ID, copyID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("implication 1girl -> copyright:bar edges = %d, want 1", n)
	}
}

// splitCategoryTag resolves a real category prefix, keeps an unknown prefix as
// part of a general-category name, and rejects the empty string.
func TestSplitCategoryTag(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	charID := lookupCategoryID(cx.DB, "character")
	cases := []struct {
		in       string
		wantCat  int64
		wantBare string
		wantOK   bool
	}{
		{"character:foo", charID, "foo", true},
		{"nier:automata", cx.GeneralCategoryID, "nier:automata", true},
		{"1girl", cx.GeneralCategoryID, "1girl", true},
		{"", 0, "", false},
	}
	for _, tc := range cases {
		cat, bare, ok := srv.splitCategoryTag(tc.in)
		if cat != tc.wantCat || bare != tc.wantBare || ok != tc.wantOK {
			t.Errorf("splitCategoryTag(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tc.in, cat, bare, ok, tc.wantCat, tc.wantBare, tc.wantOK)
		}
	}
}

// A sweep also covers the tags its implications created, in the same run:
// the implied tag's own aliases land immediately, and re-running the same
// lookup reports nothing new.
func TestPTRLookupTag_SweepsCreatedImpliedTags(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.Active().GeneralCategoryID
	parent, err := srv.tagSvc().GetOrCreateTag("1girl", generalID)
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	stub := stubPTRGraph(t, map[string]any{
		"1girl": map[string]any{
			"known":        true,
			"ideal":        "1girl",
			"aliases":      []string{"1girls"},
			"implications": []string{"girl"},
		},
		"girl": map[string]any{
			"known":   true,
			"ideal":   "girl",
			"aliases": []string{"female_child"},
		},
	}, &asked)
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})
	if st := srv.jobs.Get(); !strings.Contains(st.Summary, "added 2 alias(es) and 1 implication(s)") {
		t.Errorf("first run summary = %q, want the implied tag's alias counted in the same run", st.Summary)
	}
	var isAlias int
	if err := srv.db().Read.QueryRow(
		`SELECT is_alias FROM tags WHERE name = 'female_child'`).Scan(&isAlias); err != nil || isAlias != 1 {
		t.Fatalf("alias of the created implied tag after one run: is_alias=%d, err=%v", isAlias, err)
	}

	postPTRLookup(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/ptr-lookup", url.Values{})
	if st := srv.jobs.Get(); !strings.Contains(st.Summary, "added 0 alias(es) and 0 implication(s)") {
		t.Errorf("re-run summary = %q, want nothing new", st.Summary)
	}
}

// The search sweep resolves the current /tags filter and skips alias and
// rating rows before asking monloader.
func TestPTRLookupSearch_SkipsAliasAndRatingRows(t *testing.T) {
	srv := newTestServer(t)
	generalID := srv.Active().GeneralCategoryID
	canonical, err := srv.tagSvc().GetOrCreateTag("blonde_hair", generalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tagSvc().CreateAlias("blond_hair", generalID, canonical.ID); err != nil {
		t.Fatal(err)
	}

	var asked []string
	stub := stubPTRGraph(t, map[string]any{}, &asked)
	defer stub.Close()
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	// No filter: the sweep walks the whole catalog, including the built-in
	// rating rows the candidate query must drop.
	postPTRLookup(t, srv, "/tags/ptr-lookup-search", url.Values{"show_zero": {"1"}})

	found := map[string]bool{}
	for _, name := range asked {
		found[name] = true
	}
	if !found["blonde_hair"] {
		t.Errorf("canonical tag missing from the sweep: %v", asked)
	}
	if found["blond_hair"] {
		t.Error("alias rows must be skipped")
	}
	for _, rating := range []string{"rating:general", "rating:explicit", "general", "explicit"} {
		if found[rating] {
			t.Errorf("rating row %q must be skipped", rating)
		}
	}
}
