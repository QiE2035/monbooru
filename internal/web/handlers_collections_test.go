package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
)

func collectionMirror(t *testing.T, srv *Server, id int64) (string, sql.NullInt64) {
	t.Helper()
	var series string
	var order sql.NullInt64
	if err := srv.db().Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&series, &order); err != nil {
		t.Fatalf("read mirror for %d: %v", id, err)
	}
	return series, order
}

func membershipNames(t *testing.T, srv *Server, id int64) []string {
	t.Helper()
	cols, err := gallery.CollectionsForImage(srv.db(), id)
	if err != nil {
		t.Fatalf("CollectionsForImage(%d): %v", id, err)
	}
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	return names
}

func collPost(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	csrf := srv.csrfToken("anon")
	form.Set("_csrf", csrf)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestCollectionsPage_ListsWithPreview(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	c := seedImage(t, srv, "c.png", 12, 12)
	// Distinct from the image ids so the preview badge can be shown to
	// carry the collection position, not the id.
	seven, eight := 7, 8
	if err := gallery.SetHomeCollection(srv.db(), a, "Alpha", &seven); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), b, "Alpha", &eight); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetHomeCollection(srv.db(), c, "Beta", nil); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/collections", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /collections expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Alpha", "Beta",
		"2 images", "1 image",
		"collection-preview",
		`relations-card-id">#7<`, // the position badge, not the image id
		"btn-rename-collection", "btn-dissolve-collection",
		fmt.Sprintf("/thumbnails/%s/%d.jpg", srv.activeName, a),
		`href="/collections"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("collections page missing %q", want)
		}
	}
}

func TestCollectionsPage_FilterAndSort(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	c := seedImage(t, srv, "c.png", 12, 12)
	// Zebra has two members, Apple one: size and name orders disagree.
	if err := gallery.SetHomeCollection(srv.db(), a, "Zebra", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), b, "Zebra", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetHomeCollection(srv.db(), c, "Apple", nil); err != nil {
		t.Fatal(err)
	}

	get := func(q string) string {
		req := httptest.NewRequest("GET", "/collections"+q, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /collections%s expected 200, got %d", q, w.Code)
		}
		return w.Body.String()
	}

	body := get("?q=Zeb")
	if !strings.Contains(body, "Zebra") || strings.Contains(body, "Apple") {
		t.Errorf("q=Zeb should list Zebra only")
	}

	body = get("?sort=size")
	if strings.Index(body, "Zebra") > strings.Index(body, "Apple") {
		t.Error("sort=size should place Zebra (2) before Apple (1)")
	}

	body = get("?sort=name")
	if strings.Index(body, "Apple") > strings.Index(body, "Zebra") {
		t.Error("sort=name should place Apple before Zebra")
	}
}

func TestCollectionsPage_Empty(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/collections", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /collections expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No collections yet") {
		t.Error("empty gallery should render the empty-state line")
	}
}

func TestCollectionsPage_HonorsCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	// Mixed holds a general and an explicit member; Spicy is explicit-only.
	mixedSafe := seedImage(t, srv, "mixed_safe.png", 10, 10)
	mixedExpl := seedImage(t, srv, "mixed_expl.png", 11, 11)
	spicy := seedImage(t, srv, "spicy.png", 12, 12)
	tag := func(id int64, level string) {
		if err := cx.TagSvc.AddTagToImage(id, ratingTagIDWeb(t, cx.DB, level), false, nil); err != nil {
			t.Fatal(err)
		}
	}
	tag(mixedSafe, "general")
	tag(mixedExpl, "explicit")
	tag(spicy, "explicit")
	for _, id := range []int64{mixedSafe, mixedExpl} {
		if err := gallery.SetHomeCollection(srv.db(), id, "Mixed", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := gallery.SetHomeCollection(srv.db(), spicy, "Spicy", nil); err != nil {
		t.Fatal(err)
	}
	cx.InvalidateCaches()

	get := func(ceiling string) string {
		req := httptest.NewRequest("GET", "/collections", nil)
		if ceiling != "" {
			req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: ceiling})
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /collections (ceiling=%q) expected 200, got %d", ceiling, w.Code)
		}
		return w.Body.String()
	}

	body := get("")
	for _, want := range []string{`data-name="Mixed"`, `data-name="Spicy"`, "2 images"} {
		if !strings.Contains(body, want) {
			t.Errorf("no-ceiling collections page missing %q", want)
		}
	}

	// ceiling=sensitive drops Mixed's explicit member from the count and
	// removes Spicy entirely (its only member is hidden).
	body = get("sensitive")
	if !strings.Contains(body, `data-name="Mixed"`) || !strings.Contains(body, "1 image") {
		t.Error("ceiling=sensitive: expected Mixed with its one visible member")
	}
	if strings.Contains(body, `data-name="Spicy"`) {
		t.Error("ceiling=sensitive: explicit-only Spicy should not appear")
	}
	if strings.Contains(body, "2 images") {
		t.Error("ceiling=sensitive: Mixed should no longer count its explicit member")
	}
}

func TestRenameCollection_PlainAndCase(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	ord := 3
	if err := gallery.SetHomeCollection(srv.db(), a, "Old", &ord); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), b, "Old", nil); err != nil {
		t.Fatal(err)
	}

	w := collPost(t, srv, "/collections/rename", url.Values{"prev": {"Old"}, "name": {"New"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("rename expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Old"); len(ids) != 0 {
		t.Errorf("Old should have no members after rename, got %v", ids)
	}
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "New"); len(ids) != 2 {
		t.Errorf("New should have 2 members, got %v", ids)
	}
	if s, o := collectionMirror(t, srv, a); s != "New" || !o.Valid || o.Int64 != 3 {
		t.Errorf("a mirror = %q/%v, want New/3", s, o)
	}
	if s, _ := collectionMirror(t, srv, b); s != "New" {
		t.Errorf("b mirror = %q, want New", s)
	}

	// Case-only relabel must recase in place, not wipe the membership.
	w = collPost(t, srv, "/collections/rename", url.Values{"prev": {"New"}, "name": {"new"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("case rename expected 202, got %d", w.Code)
	}
	awaitJobsDrain(t, srv)
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "new"); len(ids) != 2 {
		t.Errorf("case rename: collection should still have 2 members, got %v", ids)
	}
	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "new" {
		t.Errorf("a membership after case rename = %v, want [new]", names)
	}
}

func TestRenameCollection_MergesIntoExisting(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	src, dst := 1, 5
	// a is homed on Src and also a member of Dst; b is in Src only.
	if err := gallery.SetHomeCollection(srv.db(), a, "Src", &src); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), a, "Dst", &dst); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetHomeCollection(srv.db(), b, "Src", nil); err != nil {
		t.Fatal(err)
	}

	w := collPost(t, srv, "/collections/rename", url.Values{"prev": {"Src"}, "name": {"Dst"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("merge rename expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Src"); len(ids) != 0 {
		t.Errorf("Src should be gone after merge, got %v", ids)
	}
	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Dst"); len(ids) != 2 {
		t.Errorf("Dst should hold both images, got %v", ids)
	}
	// a collapses to a single Dst membership; the pre-existing position wins.
	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "Dst" {
		t.Errorf("a memberships = %v, want [Dst]", names)
	}
	if s, o := collectionMirror(t, srv, a); s != "Dst" || !o.Valid || o.Int64 != 5 {
		t.Errorf("a mirror = %q/%v, want Dst/5", s, o)
	}
	if s, o := collectionMirror(t, srv, b); s != "Dst" || o.Valid {
		t.Errorf("b mirror = %q/%v, want Dst/NULL", s, o)
	}
}

func TestDissolveCollection_DropsLabelKeepsImages(t *testing.T) {
	srv := newTestServer(t)
	a := seedImage(t, srv, "a.png", 10, 10)
	b := seedImage(t, srv, "b.png", 11, 11)
	// A missing image filed under the collection must still be reached.
	res, err := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		 VALUES ('/gone/c.png', 'png', 10, 'sha_missing_c', 1, datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := res.LastInsertId()

	pos := 2
	if err := gallery.SetHomeCollection(srv.db(), a, "Gone", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), a, "Keep", &pos); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetHomeCollection(srv.db(), b, "Gone", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(srv.db(), c, "Gone", nil); err != nil {
		t.Fatal(err)
	}

	w := collPost(t, srv, "/collections/dissolve", url.Values{"collection": {"Gone"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("dissolve expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	if ids, _ := gallery.CollectionMemberIDs(srv.db(), "Gone"); len(ids) != 0 {
		t.Errorf("Gone should have no members after dissolve, got %v", ids)
	}
	// a's home rebinds to its surviving membership.
	if names := membershipNames(t, srv, a); len(names) != 1 || names[0] != "Keep" {
		t.Errorf("a memberships = %v, want [Keep]", names)
	}
	if s, o := collectionMirror(t, srv, a); s != "Keep" || !o.Valid || o.Int64 != 2 {
		t.Errorf("a mirror = %q/%v, want Keep/2", s, o)
	}
	// b and the missing c lose their only membership and clear the mirror.
	if s, _ := collectionMirror(t, srv, b); s != "" {
		t.Errorf("b mirror = %q, want empty", s)
	}
	if s, _ := collectionMirror(t, srv, c); s != "" {
		t.Errorf("c mirror = %q, want empty", s)
	}
	// No image was deleted.
	for _, id := range []int64{a, b, c} {
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, id).Scan(&n)
		if n != 1 {
			t.Errorf("image %d should still exist after dissolve", id)
		}
	}
}
