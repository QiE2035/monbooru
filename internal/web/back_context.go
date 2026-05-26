package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
)

// backContext carries the five back_* navigation params that ferry the
// gallery query / sort / page across detail, reader, pages-grid, and
// relations renders. Empty fields are skipped on encode so a URL only
// grows the params the caller actually passed in.
type backContext struct {
	Q     string
	Sort  string
	Order string
	Page  string
	Seed  string
}

// parseBackContext lifts the five fields off the request URL.
func parseBackContext(r *http.Request) backContext {
	q := r.URL.Query()
	return backContext{
		Q:     q.Get("back_q"),
		Sort:  q.Get("back_sort"),
		Order: q.Get("back_order"),
		Page:  q.Get("back_page"),
		Seed:  q.Get("back_seed"),
	}
}

// URLValues materialises the back-* fields as a url.Values, dropping
// empty entries so a missing field doesn't appear in the encoded query.
func (b backContext) URLValues() url.Values {
	v := url.Values{}
	if b.Q != "" {
		v.Set("back_q", b.Q)
	}
	if b.Sort != "" {
		v.Set("back_sort", b.Sort)
	}
	if b.Order != "" {
		v.Set("back_order", b.Order)
	}
	if b.Page != "" {
		v.Set("back_page", b.Page)
	}
	if b.Seed != "" {
		v.Set("back_seed", b.Seed)
	}
	return v
}

// QueryString returns the encoded back_* fragment prefixed with sep
// (use "?" for stand-alone hrefs, "&" for hrefs that already opened a
// query string). template.URL bypasses html/template's URL-attribute
// auto-escape so the `&` separators survive interpolation.
func (b backContext) QueryString(sep string) template.URL {
	v := b.URLValues()
	if len(v) == 0 {
		return ""
	}
	return template.URL(sep + v.Encode())
}

// DetailURL builds /images/{id}?back_q=...&... preserving every set field.
func (b backContext) DetailURL(id int64) string {
	base := fmt.Sprintf("/images/%d", id)
	v := b.URLValues()
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// GalleryURL builds "/" or "/?q=...&sort=...&..." from the back_* fields.
// The gallery URL drops the `back_` prefix - the receiver is the gallery
// list, where `q` / `sort` / `order` are the live query.
func (b backContext) GalleryURL() string {
	if b == (backContext{}) {
		return "/"
	}
	v := url.Values{}
	if b.Q != "" {
		v.Set("q", b.Q)
	}
	if b.Sort != "" {
		v.Set("sort", b.Sort)
	}
	if b.Order != "" {
		v.Set("order", b.Order)
	}
	if b.Page != "" {
		v.Set("page", b.Page)
	}
	if b.Seed != "" {
		v.Set("seed", b.Seed)
	}
	return "/?" + v.Encode()
}

// ReaderQS returns the two query fragments the reader template needs:
// a stand-alone `?...` for the detail-page back link, and a `&...`
// tail for reader-internal page-flip links that already open `?page=N`.
// Both are empty when no back_* is set and fromPages is false.
func (b backContext) ReaderQS(fromPages bool) (template.URL, template.URL) {
	v := b.URLValues()
	if fromPages {
		v.Set("from", "pages")
	}
	if len(v) == 0 {
		return "", ""
	}
	enc := v.Encode()
	return template.URL("?" + enc), template.URL("&" + enc)
}
