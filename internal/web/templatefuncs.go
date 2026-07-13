package web

import (
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
)

// templateFuncs is the FuncMap every template render sees. Lives apart
// from NewServer so router.go stays routes + middleware + server
// lifecycle.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"seq": func(start, end int) []int {
			r := make([]int, 0, end-start+1)
			for i := start; i <= end; i++ {
				r = append(r, i)
			}
			return r
		},
		"add": func(a, b int) int { return a + b },
		// urlQ percent-encodes a query value with uppercase hex pairs so
		// the links the sidebar emits match the case the browser writes
		// back into the address bar (browsers normalize to uppercase per
		// RFC 3986). Without this the user's autocomplete history grows
		// two entries per logical query (one with lowercase hex, one
		// uppercase). url.QueryEscape emits lowercase; we re-case the
		// %XX sequences without touching the surrounding letters.
		//
		// Returns template.URL so html/template's href-context URL
		// autoescaper leaves the value alone. As a plain string it would
		// re-percent-encode every `%`, double-encoding the link and
		// turning `folder:"path"` into a literal query with no matches.
		"urlQ": func(s string) template.URL {
			return template.URL(uppercasePercentEscapes(url.QueryEscape(s)))
		},
		// qval backslash-escapes a label so it survives interpolation
		// into a quoted `key:"<value>"` search term (collection / source
		// links). The parser's unescapeQuoted reverses it, so a label
		// containing a double-quote round-trips instead of truncating the
		// query at the inner quote.
		"qval": search.QuoteValue,
		"sub":  func(a, b int) int { return a - b },
		"list": func(vs ...any) []any { return vs },
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		"groupByCategory": func(tagList []models.Tag) []tagGroup {
			return groupOrdered(tagList, nil,
				func(t models.Tag) string { return t.CategoryName },
				func(t models.Tag) *tagGroup { return &tagGroup{Name: t.CategoryName, Color: t.CategoryColor} },
				func(g *tagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref64": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref64f": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"phashHex": func(p *int64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%016x", uint64(*p))
		},
		// The "ptr" source has no post page: its fetch action is a hash
		// lookup instead of a url refetch, so templates branch on the label.
		"isPTRSite": func(site string) bool {
			return strings.EqualFold(strings.TrimSpace(site), "ptr")
		},
		"groupByImageSource": func(tagList []models.ImageTag) []imageTagSourceGroup {
			// Manual tags split by source: plain UI adds (empty tagger_name)
			// land in the "user" bucket; API-supplied sources each get their
			// own "Tags added by <source>" subsection. Auto rows keep the
			// existing per-tagger grouping with the "auto-tagger" suffix.
			// is_implied rows skip every source bucket - they render
			// together with aliases inside the collapsed wrapper at the
			// bottom of the under-image list.
			var userTags []models.ImageTag
			byUserSource := map[string]*imageTagSourceGroup{}
			var userSourceOrder []string
			byTagger := map[string]*imageTagSourceGroup{}
			var order []string
			for _, t := range tagList {
				if t.IsImplied {
					continue
				}
				if !t.IsAuto {
					if t.TaggerName == "" {
						userTags = append(userTags, t)
						continue
					}
					key := t.TaggerName
					if _, ok := byUserSource[key]; !ok {
						title := "Tags added by " + key
						if strings.EqualFold(key, "ptr") {
							// The source label stays "ptr" (search, image_sources);
							// only the heading spells it out.
							title = "Tags added by the Public Tag Repository"
						}
						userSourceOrder = append(userSourceOrder, key)
						byUserSource[key] = &imageTagSourceGroup{
							Source: key,
							Title:  title,
						}
					}
					byUserSource[key].Tags = append(byUserSource[key].Tags, t)
					continue
				}
				key := t.TaggerName
				if key == "" {
					key = "auto-tagger"
				}
				if _, ok := byTagger[key]; !ok {
					order = append(order, key)
					byTagger[key] = &imageTagSourceGroup{
						Source: key,
						Title:  "Tags added by the " + key + " auto-tagger",
					}
				}
				byTagger[key].Tags = append(byTagger[key].Tags, t)
			}
			out := []imageTagSourceGroup{}
			if len(userTags) > 0 {
				out = append(out, imageTagSourceGroup{
					Source: "user",
					Title:  "Tags added by the user",
					Tags:   userTags,
				})
			}
			for _, k := range userSourceOrder {
				out = append(out, *byUserSource[k])
			}
			for _, k := range order {
				g := byTagger[k]
				// Auto-tagger subgroups read more naturally ordered by the
				// tagger's own confidence: the tags the model was most sure
				// of sit at the top. User tags above keep the existing
				// alphabetical-by-category-then-usage order.
				sort.SliceStable(g.Tags, func(i, j int) bool {
					ci, cj := 0.0, 0.0
					if g.Tags[i].Confidence != nil {
						ci = *g.Tags[i].Confidence
					}
					if g.Tags[j].Confidence != nil {
						cj = *g.Tags[j].Confidence
					}
					return ci > cj
				})
				out = append(out, *g)
			}
			return out
		},
		"impliedFromImageTags": func(tagList []models.ImageTag) []models.ImageTag {
			var out []models.ImageTag
			for _, t := range tagList {
				if t.IsImplied {
					out = append(out, t)
				}
			}
			return out
		},
		"autoConfPct": func(c *float64) string {
			if c == nil {
				return ""
			}
			return strconv.Itoa(int(*c * 100))
		},
		"groupByImageTags": func(tagList []models.ImageTag) []imageTagGroup {
			// Sidebar consumer: skip implied rows. The user asked for them
			// to render only in the under-image list (less visible there),
			// not in the per-image sidebar where every tag would compete
			// for the same column.
			out := groupOrdered(tagList,
				func(t models.ImageTag) bool { return t.IsImplied },
				func(t models.ImageTag) string { return t.Category },
				func(t models.ImageTag) *imageTagGroup { return &imageTagGroup{Name: t.Category, Color: t.Color} },
				func(g *imageTagGroup, t models.ImageTag) { g.Tags = append(g.Tags, t) })
			// Lift rating to the top so the effective rating sits where
			// the eye lands first.
			for i, g := range out {
				if g.Name == "rating" && i > 0 {
					out = append([]imageTagGroup{g}, append(out[:i], out[i+1:]...)...)
					break
				}
			}
			return out
		},
		"cancelTitle": func(jobType string) string {
			// Tooltip for the job-status × button. Only the job types that
			// observe ctx.Done() in their worker loop appear here.
			switch jobType {
			case "autotag":
				return "Stop auto-tagging"
			case "sync":
				return "Stop syncing"
			case "delete":
				return "Stop deleting"
			case "re-extract":
				return "Stop re-extraction"
			case "rebuild-thumbs":
				return "Stop thumbnail rebuild"
			case "prune-thumbs":
				return "Stop thumbnail prune"
			case "phash":
				return "Stop phash backfill"
			case "relations":
				return "Stop find-pairs"
			case "move":
				return "Stop moving"
			case "tag":
				return "Stop tagging"
			}
			return "Stop"
		},
		"humanBytes": humanBytesFmt,
		// localTime renders a stored-UTC timestamp in the process timezone
		// (time.Local, driven by TZ) so displayed times match the operator's
		// wall clock. Storage stays UTC; only the display converts.
		"localTime": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localTimePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"browseSortLabel": func(s string) string {
			switch s {
			case "recent":
				return "Recent"
			case "size":
				return "Size"
			case "original_added":
				return "Original added"
			case "length":
				return "Length"
			case "newest_member":
				return "Newest member"
			}
			return s
		},
		"isLongValue": func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n\r")
		},
		"schedDuration": func(d time.Duration) string {
			// Round to the nearest second for anything over 1s; keep
			// millisecond precision below so sub-second scheduler passes
			// (the typical case on an idle gallery) still render usefully.
			if d >= time.Second {
				return d.Round(time.Second).String()
			}
			return d.Round(time.Millisecond).String()
		},
		"minusDuration": func(a, b time.Duration) time.Duration {
			return a - b
		},
		"int64Duration": func(d time.Duration) int64 {
			return int64(d)
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"comfyRefTarget": func(s string) string {
			// Displayed ComfyUI references start with "→ " followed by the
			// referenced node's key. Strip the arrow+space so the template
			// can build `href="#comfy-node-<key>"` for in-page navigation.
			return strings.TrimPrefix(s, "→ ")
		},
		"hasPrefix": func(s, prefix string) bool {
			return strings.HasPrefix(s, prefix)
		},
		// urlDomain returns the host of a URL (without a leading "www.") for
		// display; the full URL still drives the link's href. Falls back to
		// the input when it doesn't parse as an absolute URL with a host.
		"urlDomain": func(s string) string {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return s
			}
			return strings.TrimPrefix(u.Host, "www.")
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n])
		},
		// hasFavFilter reports whether the search query contains a `fav:true`
		// token, regardless of position or surrounding tags. Drives the gallery
		// header's ♥ toggle's active class so the button doesn't go inactive
		// the moment the user combines `fav:true` with any other tag.
		"hasFavFilter": func(query string) bool {
			for _, tok := range strings.Fields(query) {
				if strings.EqualFold(tok, "fav:true") {
					return true
				}
			}
			return false
		},
		"pageLoadMs": func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return time.Since(t).Milliseconds()
		},
	}
}
