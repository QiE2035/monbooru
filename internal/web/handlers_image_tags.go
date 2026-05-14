package web

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
)

// catTag pairs a resolved category ID with a tag name for creation/application.
type catTag struct {
	catID int64
	name  string
}

// parseTagInput parses multi-token tag input.
//
// Tokens are separated by whitespace. Each token becomes its own tag: a
// bare word, a "category:name" pair, or a double-quoted span whose
// internal spaces are collapsed to underscores (so `"red hair"` →
// `red_hair`). Quotes can follow a category prefix
// (`artist:"john doe"`).
//
// Examples:
//
//	red hair                 -> [{general, "red"}, {general, "hair"}]
//	"red hair" blue_eyes     -> [{general, "red_hair"}, {general, "blue_eyes"}]
//	artist:"john doe" 1girl  -> [{artist, "john_doe"}, {general, "1girl"}]
func (s *Server) parseTagInput(tagInput string) ([]catTag, string) {
	tokens, err := splitTagTokens(tagInput)
	if err != nil {
		return nil, err.Error()
	}

	// general category id is cached on galleryCtx at open time so this
	// hot path doesn't re-query the immutable built-in row.
	var generalID int64
	if cx := s.Active(); cx != nil {
		generalID = cx.GeneralCategoryID
	}

	var catTags []catTag
	var rejected []string
	for _, tok := range tokens {
		name := tok.name
		if idx := strings.Index(name, ":"); idx > 0 {
			catName := name[:idx]
			tagName := name[idx+1:]
			var catID int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tag_categories WHERE name=?`, catName,
			).Scan(&catID); err == nil {
				if tagName == "" {
					// `general:` (known category, empty name) was a silent
					// drop; surface it like the other malformed-token cases
					// so the user sees what their input did.
					rejected = append(rejected, "rejected: "+name+": empty tag name after category prefix")
					continue
				}
				catTags = append(catTags, catTag{catID, tagName})
				continue
			}
			// Prefix isn't a known category; treat the whole token as a
			// literal general-category tag (e.g. "nier:automata").
		}
		catTags = append(catTags, catTag{generalID, name})
	}

	return catTags, strings.Join(rejected, "; ")
}

// parsedTagToken is one tokenizer output: its resolved name.
type parsedTagToken struct {
	name string
}

// splitTagTokens splits tag-input into whitespace-separated tokens while
// respecting double-quoted spans. Inside a quoted span, internal spaces
// are replaced with underscores. Quoted spans may be preceded by a
// category prefix (`artist:"john doe"`). Unterminated quotes return an
// error.
func splitTagTokens(s string) ([]parsedTagToken, error) {
	var tokens []parsedTagToken
	var buf strings.Builder
	quoted := false
	inToken := false

	flush := func() {
		if !inToken {
			return
		}
		tokens = append(tokens, parsedTagToken{name: buf.String()})
		buf.Reset()
		inToken = false
	}

	for _, r := range s {
		if r == '"' {
			quoted = !quoted
			inToken = true
			continue
		}
		if quoted {
			if r == ' ' || r == '\t' {
				buf.WriteRune('_')
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		buf.WriteRune(r)
		inToken = true
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in tag input")
	}
	flush()
	return tokens, nil
}

func (s *Server) addTagToImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tag"))
	if tagInput == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}

	catTags, parseErrMsg := s.parseTagInput(tagInput)

	var added, rejected, dupes []string
	var promotedTokens []string
	var displacedRatings []string
	mutated := false

	// Resolve every token up front so the inserts ride one writer
	// round-trip; a 50-token paste pays one transaction instead of N.
	type resolved struct {
		name string
		tag  *models.Tag
	}
	prepared := make([]resolved, 0, len(catTags))
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			logx.Warnf("add tag %q: %v", ct.name, err)
			rejected = append(rejected, ct.name+": "+err.Error())
			continue
		}
		prepared = append(prepared, resolved{name: ct.name, tag: tag})
	}

	if len(prepared) > 0 {
		tagIDs := make([]int64, len(prepared))
		for i, p := range prepared {
			tagIDs[i] = p.tag.ID
		}
		results, err := s.tagSvc().AddTagsToOneImage(id, tagIDs)
		if err != nil {
			logx.Warnf("batch add tags to image %d: %v", id, err)
			// On batch failure surface a single rejection covering every
			// prepared token so the user knows none landed.
			for _, p := range prepared {
				rejected = append(rejected, p.name+": "+err.Error())
			}
		} else {
			for i, res := range results {
				name := prepared[i].name
				if res.Added || res.Promoted {
					mutated = true
				}
				if res.Added && !res.Promoted {
					added = append(added, name)
				}
				if !res.Added && !res.Promoted {
					dupes = append(dupes, name)
				}
				if res.Promoted {
					promotedTokens = append(promotedTokens, name)
				}
				displacedRatings = append(displacedRatings, res.DisplacedRatings...)
			}
		}
	}

	if mutated {
		s.Active().InvalidateCaches()
	}

	// Distinguish "everything went in" from "some tokens failed" so a
	// pasted multi-token input doesn't leave the user diffing the under-
	// image list against their string. The input is cleared on full
	// success and on a clean partial (some applied, some duplicates):
	// the user can read the live tag list to confirm what's there. It
	// stays populated only when at least one token was rejected, so the
	// user can edit and resubmit.
	//
	// Three flash buckets the template renders in three colours: red
	// (errors only), orange (mixed success + reject), green (success
	// only). Build the parts once, then route them.
	addedPart := func() string {
		if len(added) > 0 {
			return "added: " + strings.Join(added, ", ")
		}
		return ""
	}()
	promotedPart := func() string {
		if len(promotedTokens) > 0 {
			return "promoted to user tag: " + strings.Join(promotedTokens, ", ")
		}
		return ""
	}()
	dupesPart := func() string {
		if mutated && len(dupes) > 0 {
			return "already on image: " + strings.Join(dupes, ", ")
		}
		return ""
	}()
	displacedPart := func() string {
		if len(displacedRatings) > 0 {
			return "replaced rating " + strings.Join(displacedRatings, ", ")
		}
		return ""
	}()
	rejectedPart := func() string {
		if len(rejected) > 0 {
			return "rejected: " + strings.Join(rejected, "; ")
		}
		return ""
	}()

	joinNonEmpty := func(parts ...string) string {
		out := parts[:0]
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, "; ")
	}

	var addErrMsg, addWarnMsg, addOkMsg string
	switch {
	case parseErrMsg != "" && !mutated && len(rejected) == 0:
		addErrMsg = parseErrMsg
	case mutated && (len(rejected) > 0 || parseErrMsg != ""):
		// Mixed outcome: render in warn-orange and surface both
		// successes and the rejected tokens.
		addWarnMsg = joinNonEmpty(parseErrMsg, addedPart, promotedPart, dupesPart, displacedPart, rejectedPart)
	case len(rejected) > 0:
		addErrMsg = joinNonEmpty(parseErrMsg, rejectedPart)
	case len(dupes) > 0 && !mutated && parseErrMsg == "":
		// Whole submit hit only existing tags; preserve the prior
		// soft-error feedback so the user sees something happened.
		addErrMsg = "tag already on image: " + strings.Join(dupes, ", ")
	default:
		addOkMsg = joinNonEmpty(addedPart, promotedPart, dupesPart, displacedPart)
	}
	s.renderTagListWithSidebar(w, r, id, addErrMsg, addWarnMsg, addOkMsg, len(rejected) == 0 && parseErrMsg == "")
}

// aliasesForImageTags returns the alias rows pointing at any non-implied
// tag carried by the image, ordered by name. Used by both the full
// detail render and the htmx tag-list refresh so the "Aliases" group at
// the bottom of the under-image list stays in sync.
func (s *Server) aliasesForImageTags(imageTags []models.ImageTag) []models.Tag {
	if len(imageTags) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		if t.IsImplied {
			continue
		}
		ids = append(ids, t.TagID)
	}
	if len(ids) == 0 {
		return nil
	}
	byCanon, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil
	}
	var out []models.Tag
	for _, list := range byCanon {
		out = append(out, list...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderTagListWithSidebar renders the image tag list partial and always emits
// OOB swaps of the detail sidebar and danger zone so tag groups and remove-tag
// buttons stay in sync without a page reload.
// errMsg / warnMsg / okMsg are shown as inline flashes if non-empty (red,
// orange, green); clearInput resets the add-tag input.
func (s *Server) renderTagListWithSidebar(w http.ResponseWriter, r *http.Request, id int64, errMsg, warnMsg, okMsg string, clearInput bool) {
	imageTags, _ := s.tagSvc().GetImageTags(id)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}
	var folderPath string
	_ = s.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&folderPath)
	q := r.URL.Query()
	s.renderTemplate(w, "partials/tag_list.html", map[string]any{
		"ImageID":       id,
		"ImageTags":     imageTags,
		"Aliases":       s.aliasesForImageTags(imageTags),
		"SidebarTags":   true,
		"DangerZone":    true,
		"HasUserTags":   hasUserTags,
		"ImageTaggers":  distinctAutoTaggerNames(imageTags),
		"BackQuery":     q.Get("back_q"),
		"BackSort":      q.Get("back_sort"),
		"BackOrder":     q.Get("back_order"),
		"BackPage":      q.Get("back_page"),
		"BackSeed":      q.Get("back_seed"),
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"EditMode":      true,
		"ErrMsg":        errMsg,
		"WarnMsg":       warnMsg,
		"OkMsg":         okMsg,
		"ClearInput":    clearInput,
		"CurrentFolder": folderPath,
	})
}

// removeAutoTagsFromImageHandler removes auto-tagged rows from one image,
// optionally filtered by the caller-supplied `taggers` query parameter
// (comma-separated tagger names). Empty filter removes every auto-tag.
func (s *Server) removeAutoTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	raw := r.URL.Query().Get("taggers")
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if err := s.tagSvc().RemoveAutoTagsFromImage(id, names); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}

func (s *Server) removeUserTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveUserTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}

func (s *Server) removeAllTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveAllTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}

func (s *Server) removeTagFromImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	tagIDStr := r.PathValue("tagID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagID, err := strconv.ParseInt(tagIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad tagID", http.StatusBadRequest)
		return
	}

	if err := s.tagSvc().RemoveTagFromImage(id, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}

func (s *Server) changeTagCategory(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	catIDStr := r.FormValue("category_id")
	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad category_id", http.StatusBadRequest)
		return
	}
	// Route through the tag service for validation and consistency.
	if err := s.tagSvc().ChangeTagCategory(id, catID); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// cat:/category-qualified searches resolve via the moved tag's
	// new category, so cached match-id lists for those queries can't
	// survive the move.
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Category updated.</div>`))
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) getImageTagsHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}
