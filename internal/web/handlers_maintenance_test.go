package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTagCategoryConflicts(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	tagSvc := srv.tagSvc()
	generalID := lookupCategoryID(cx.DB, "general")
	characterID := lookupCategoryID(cx.DB, "character")

	// Same name in two categories is a conflict; a name in one is not.
	for _, tc := range []struct {
		name string
		cat  int64
	}{{"nintendo", generalID}, {"nintendo", characterID}, {"solo", generalID}} {
		if _, err := tagSvc.GetOrCreateTag(tc.name, tc.cat); err != nil {
			t.Fatalf("GetOrCreateTag(%q): %v", tc.name, err)
		}
	}

	form := url.Values{"_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/settings/maintenance/tag-conflicts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<strong>1</strong> tag name span") {
		t.Errorf("diagnostic should count the single conflicting name; got %s", body)
	}
	if !strings.Contains(body, `href="/tags?conflicts=1"`) {
		t.Errorf("diagnostic should link the Tags page's Conflicts filter; got %s", body)
	}

	// The /tags Conflicts filter lists both sides of the split, name-
	// sorted so the pair sits adjacent, and leaves single-category
	// names out.
	listReq := httptest.NewRequest("GET", "/tags?conflicts=1", nil)
	listW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listW, listReq)
	listBody := listW.Body.String()
	if got := strings.Count(listBody, `>nintendo</a>`); got != 2 {
		t.Errorf("conflicts filter should list both nintendo rows, got %d", got)
	}
	if strings.Contains(listBody, `>solo</a>`) {
		t.Errorf("single-category tag solo should not pass the conflicts filter")
	}
}

func TestTagCategoryConflicts_None(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/settings/maintenance/tag-conflicts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "No tags share a name") {
		t.Errorf("expected the empty-state message; got %s", body)
	}
}
