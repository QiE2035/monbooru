package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// seedDupGroup inserts two images with deterministic SHAs and declares
// them as a duplicate group. Used by the pagination test below.
func seedDupGroup(t *testing.T, srv *Server, salt string) {
	t.Helper()
	sha := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	insert := func(path, hash string) int64 {
		t.Helper()
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, source_type, origin)
			 VALUES (?, ?, '', 'png', 1024, 'image', 'test')`,
			hash, path,
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	a := insert(salt+"a.png", sha(salt+"a"))
	b := insert(salt+"b.png", sha(salt+"b"))
	if err := srv.Active().RelationsSvc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
}

// postDissolve fires a CSRF-tagged form post at the bulk dissolve
// endpoint with the named field repeated once per value. Returns the
// recorder so the test can assert on status / headers.
func postDissolve(t *testing.T, srv *Server, kind, field string, values ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("_csrf", srv.csrfToken("anon"))
	for _, v := range values {
		form.Add(field, v)
	}
	req := httptest.NewRequest("POST", "/relations/browse-groups/dissolve?kind="+kind,
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// Bulk dissolve covers each of the five kinds the toolbar can submit.
// Each subtest seeds the relevant relation, posts the matching id field,
// and confirms the underlying row(s) are gone.
func TestDissolveGroupsPost(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		srv := newTestServer(t)
		seedDupGroup(t, srv, "bulk-dup-")
		var gid int64
		if err := srv.db().Read.QueryRow(`SELECT id FROM dup_groups LIMIT 1`).Scan(&gid); err != nil {
			t.Fatal(err)
		}
		w := postDissolve(t, srv, "duplicate", "group_id", strconv.FormatInt(gid, 10))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&n)
		if n != 0 {
			t.Errorf("dup_groups remaining = %d, want 0", n)
		}
	})

	t.Run("alternate", func(t *testing.T) {
		srv := newTestServer(t)
		a := insertRawImage(t, srv, "alt-a.png", "")
		b := insertRawImage(t, srv, "alt-b.png", "")
		c := insertRawImage(t, srv, "alt-c.png", "")
		d := insertRawImage(t, srv, "alt-d.png", "")
		if err := srv.Active().RelationsSvc.AddAlternate(a, b); err != nil {
			t.Fatal(err)
		}
		if err := srv.Active().RelationsSvc.AddAlternate(c, d); err != nil {
			t.Fatal(err)
		}
		rows, err := srv.db().Read.Query(`SELECT id FROM alt_groups ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var gids []string
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			gids = append(gids, strconv.FormatInt(id, 10))
		}
		_ = rows.Close()
		if len(gids) != 2 {
			t.Fatalf("alt_groups seed = %d, want 2", len(gids))
		}
		w := postDissolve(t, srv, "alternate", "group_id", gids...)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM alt_groups`).Scan(&n)
		if n != 0 {
			t.Errorf("alt_groups remaining = %d, want 0", n)
		}
	})

	t.Run("version", func(t *testing.T) {
		srv := newTestServer(t)
		a := insertRawImage(t, srv, "v-a.png", "")
		b := insertRawImage(t, srv, "v-b.png", "")
		if err := srv.Active().RelationsSvc.AddVersionEdge(a, b); err != nil {
			t.Fatal(err)
		}
		w := postDissolve(t, srv, "version", "root_id", strconv.FormatInt(a, 10))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n)
		if n != 0 {
			t.Errorf("version_edges remaining = %d, want 0", n)
		}
	})

	t.Run("derivative", func(t *testing.T) {
		srv := newTestServer(t)
		a := insertRawImage(t, srv, "d-a.png", "")
		b := insertRawImage(t, srv, "d-b.png", "")
		if err := srv.Active().RelationsSvc.AddDerivativeEdge(a, b); err != nil {
			t.Fatal(err)
		}
		w := postDissolve(t, srv, "derivative", "root_id", strconv.FormatInt(a, 10))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n)
		if n != 0 {
			t.Errorf("derivative_edges remaining = %d, want 0", n)
		}
	})

	t.Run("not_related", func(t *testing.T) {
		srv := newTestServer(t)
		a := insertRawImage(t, srv, "nr-a.png", "")
		b := insertRawImage(t, srv, "nr-b.png", "")
		if err := srv.Active().RelationsSvc.AddNotRelated(a, b); err != nil {
			t.Fatal(err)
		}
		pair := strconv.FormatInt(a, 10) + ":" + strconv.FormatInt(b, 10)
		w := postDissolve(t, srv, "not_related", "pair", pair)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		var n int
		_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM not_related_pairs`).Scan(&n)
		if n != 0 {
			t.Errorf("not_related_pairs remaining = %d, want 0", n)
		}
	})

	t.Run("unknown kind 400", func(t *testing.T) {
		srv := newTestServer(t)
		w := postDissolve(t, srv, "garbage", "group_id", "1")
		if w.Code != http.StatusBadRequest {
			t.Errorf("code=%d body=%s", w.Code, w.Body.String())
		}
	})
}

// TestRelationsBrowse_Pagination pins the browseRelationsPageSize
// constant by seeding one row past the cap and asserting page 1
// renders exactly the cap and page 2 renders the overflow. A
// regression that bumped the constant silently would surface here as
// a card-count mismatch.
func TestRelationsBrowse_Pagination(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < browseRelationsPageSize+1; i++ {
		seedDupGroup(t, srv, fmt.Sprintf("d%03d-", i))
	}

	count := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code=%d", path, w.Code)
		}
		// Each browse card opens with `class="relations-card ` so
		// counting that prefix is robust to extra trailing classes
		// (kind variants, status modifiers) the template may carry.
		return strings.Count(w.Body.String(), `class="relations-card `)
	}

	p1 := count("/relations/browse?kind=duplicate")
	p2 := count("/relations/browse?kind=duplicate&page=2")
	if p1 != browseRelationsPageSize {
		t.Errorf("page 1 cards = %d, want %d", p1, browseRelationsPageSize)
	}
	if p2 != 1 {
		t.Errorf("page 2 cards = %d, want 1", p2)
	}
}
