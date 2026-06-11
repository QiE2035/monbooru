package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tagger"
)

// taggerRow is the per-template render shape for one row of the
// Auto-Tagger settings table. It unifies installed taggers and catalog
// ghosts so the template iterates a single list. Supported rows (i.e.
// in the catalog) carry precomputed host + docker install snippets so
// the Instructions cell can open a per-row dialog without the template
// touching shell quoting.
type taggerRow struct {
	Name                string
	Description         string
	Available           bool
	Reason              string
	Enabled             bool
	ConfidenceThreshold float64
	ThresholdSummary    string
	GallerySummary      string
	Installed           bool
	Supported           bool
	HostCommand         string
	DockerCommand       string
}

// thresholdRow is the per-category render shape for the per-tagger
// Configure dialog. Override is the live category_thresholds value; an
// empty Override falls back to the global threshold and the input
// renders a placeholder instead of a value. MaxTags is the live
// per_category_top_k value formatted as a string ("" = use default,
// "0" = uncapped). Disabled mirrors disabled_categories membership; a
// disabled category emits nothing regardless of its threshold.
// DefaultThreshold / DefaultMaxTags are the catalog-seeded values for
// this category (empty when the catalog seeds none); the per-row Reset
// restores the inputs to these so it lands on the same state the
// dialog-level "Reset to defaults" would, instead of blanking the cell.
type thresholdRow struct {
	Category         string
	Override         string // "" when no override; formatted "%.2f" otherwise
	MaxTags          string // "" when no override; integer string otherwise
	MaxDefault       int    // default cap surfaced as the input placeholder
	Color            string // tag_categories.color, surfaced as a 1px dot
	Disabled         bool   // category is muted (in disabled_categories)
	DefaultThreshold string // catalog default threshold; "" = no catalog override
	DefaultMaxTags   string // catalog default top-K; "" = no catalog override
}

// taggerGalleryRow is the per-gallery render shape for the per-tagger
// Galleries dialog: one entry per configured gallery, with Checked =
// true when the tagger's Galleries list contains this name (or is
// empty/missing, meaning "every gallery").
type taggerGalleryRow struct {
	Name    string
	Checked bool
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	newUseCUDA := r.FormValue("use_cuda") == "on"
	// Probe CUDA before persisting the enable so the user sees any library/GPU
	// issue immediately instead of waiting for a tagger run to fail. ORT env
	// init is not re-entrant so refuse while a tagger job is holding it.
	if newUseCUDA && !s.cfg.Tagger.UseCUDA {
		if s.jobs.IsRunning() {
			writeInlineFlash(w, "err", "A job is running; try again when it finishes.")
			return
		}
		if err := tagger.CheckCUDAAvailable(); err != nil {
			writeInlineFlash(w, "err", "Cannot enable GPU: "+err.Error())
			return
		}
	}

	s.cfgMu.Lock()
	cudaChanged := s.cfg.Tagger.UseCUDA != newUseCUDA
	s.cfg.Tagger.UseCUDA = newUseCUDA
	if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
		s.cfg.Tagger.Parallel = n
	}
	if v := r.FormValue("idle_release_after_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.cfg.Tagger.IdleReleaseAfterMinutes = n
		}
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	// Drop the cached ORT session so the freed RAM is visible immediately
	// rather than after idle_release_after_minutes elapses.
	if cudaChanged {
		tagger.ReleaseAll()
	}
	logx.Infof("settings: tagger updated (use_cuda=%t)", s.cfg.Tagger.UseCUDA)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"UseCUDA": s.cfg.Tagger.UseCUDA,
		"OOB":     true,
	})
}

// settingsTaggerEnablePost flips one tagger's enabled flag to true without
// going through the full tagger form. Mirrors settingsTaggerDisablePost.
func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), true)
}

// settingsTaggerDisablePost flips one tagger's enabled flag to false without
// going through the full tagger form. An HX-Refresh header re-renders the
// settings page so the row's enabled state and Actions column reflect the
// new state.
func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), false)
}

// updateTagger applies mutate to the named TaggerInstance under cfgMu,
// seeding a fresh entry from the on-disk catalog when one doesn't exist
// yet, then persists the config to disk. Returns the saveConfig error
// or nil. Callers that need to surface a save failure to the operator
// pass its error string through their usual flash helper.
func (s *Server) updateTagger(name string, mutate func(*config.TaggerInstance)) error {
	s.cfgMu.Lock()
	found := false
	for i := range s.cfg.Tagger.Taggers {
		if s.cfg.Tagger.Taggers[i].Name == name {
			mutate(&s.cfg.Tagger.Taggers[i])
			found = true
			break
		}
	}
	if !found {
		catalog := catalogEntryByName(s.cfg.Paths.ModelPath, name)
		seeded := tagger.SeedTaggerInstance(name, false, catalog)
		mutate(&seeded)
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, seeded)
	}
	s.cfgMu.Unlock()
	return s.saveConfig()
}

// applyTaggerEnabled flips a tagger's Enabled flag, seeding a TOML
// entry from the on-disk catalog when one doesn't exist yet so the
// preference persists across disable/enable round trips.
func (s *Server) applyTaggerEnabled(w http.ResponseWriter, name string, enabled bool) {
	if err := tagger.ValidateTaggerName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.Enabled = enabled
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	logx.Infof("settings: tagger %q %s", name, verb)
	w.Header().Set("HX-Refresh", "true")
	writeInlineFlash(w, "ok", "Tagger "+name+" "+verb+".")
}

// settingsTaggerThresholdsGet renders the dialog body for one tagger's
// thresholds: a global slot plus one row per emitted category, with a
// "+ category" select listing the rest of tag_categories so an operator
// can override a category the model wouldn't otherwise emit (a
// dispatch rule could route something into it). HTMX lazy-loads the
// body via hx-get on first dialog open.
func (s *Server) settingsTaggerThresholdsGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	rows, global, ok := s.thresholdDialogData(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	csrf := s.csrfToken(sessionFromContext(r.Context()))
	s.renderTemplate(w, "partials/tagger_thresholds_dialog.html", map[string]any{
		"Name":      name,
		"Global":    global,
		"Rows":      rows,
		"CSRFToken": csrf,
	})
}

// settingsTaggerThresholdsPost saves the per-tagger threshold form. The
// global threshold is required (input type=number, validated client-side
// too); a category row with an empty Threshold or Max-tags value clears
// the matching override.
//
// On validation error the inline flash inside the dialog is updated and
// the dialog stays open. On success the dialog closes (via the
// `tagger-saved` HX-Trigger event), the parent settings page's
// `#flash-tagger` carries the confirmation, and the row's summary text
// is OOB-swapped to reflect the new values without a page reload.
func (s *Server) settingsTaggerThresholdsPost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	globalRaw := strings.TrimSpace(r.FormValue("global_threshold"))
	global, err := strconv.ParseFloat(globalRaw, 64)
	if err != nil || global < 0 || global > 1 {
		writeInlineFlash(w, "err", "Global threshold must be between 0 and 1.")
		return
	}
	overrides := map[string]float64{}
	topK := map[string]int{}
	var disabled []string
	for _, cat := range r.Form["category"] {
		cat = strings.TrimSpace(cat)
		if cat == "" {
			continue
		}
		raw := strings.TrimSpace(r.FormValue("threshold_" + cat))
		if raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 || v > 1 {
				writeInlineFlash(w, "err", "Threshold for "+cat+" must be between 0 and 1.")
				return
			}
			overrides[cat] = v
		}
		rawK := strings.TrimSpace(r.FormValue("maxtags_" + cat))
		if rawK != "" {
			n, err := strconv.Atoi(rawK)
			if err != nil || n < 0 {
				writeInlineFlash(w, "err", "Max tags for "+cat+" must be 0 or higher.")
				return
			}
			topK[cat] = n
		}
		// Every row submits its category hidden input; the per-row Enable
		// checkbox is only present when ticked. A category whose Enable box
		// is absent is muted. The dialog ships every category enabled by
		// default, so disabling is the deliberate act.
		if r.FormValue("enable_"+cat) == "" {
			disabled = append(disabled, cat)
		}
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = global
		if len(overrides) > 0 {
			t.CategoryThresholds = overrides
		} else {
			t.CategoryThresholds = nil
		}
		if len(topK) > 0 {
			t.PerCategoryTopK = topK
		} else {
			t.PerCategoryTopK = nil
		}
		t.DisabledCategories = disabled
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q thresholds updated (global=%.2f, %d threshold overrides, %d top-K overrides, %d disabled)", name, global, len(overrides), len(topK), len(disabled))
	summary := taggerThresholdSummary(global, overrides, disabled)
	setTaggerSavedTrigger(w, "tagger-thresh-"+name)
	_, _ = fmt.Fprintf(w,
		`<span id="tagger-thresh-summary-%s" hx-swap-oob="true">%s</span>`+
			`<div id="flash-tagger" hx-swap-oob="true"><div class="flash flash-ok">Tagger %s thresholds saved.</div></div>`,
		html.EscapeString(name), html.EscapeString(summary), html.EscapeString(name))
}

// setTaggerSavedTrigger fires a JS-side `tagger-saved` event with the
// dialog id to close. The shared shape lets one body listener serve
// every per-tagger config dialog (thresholds, galleries, future ones).
func setTaggerSavedTrigger(w http.ResponseWriter, dialogID string) {
	payload, _ := json.Marshal(map[string]any{
		"tagger-saved": map[string]any{"dialog": dialogID},
	})
	w.Header().Set("HX-Trigger", string(payload))
}

// settingsTaggerThresholdsResetPost wipes per-tagger threshold and
// top-K overrides and rebases the global threshold to the catalog
// default (or the package fallback when no catalog entry exists).
// Renders the dialog body afresh so the inputs reflect the reset values
// without a save round-trip; the row summary is OOB-swapped so the
// parent table updates immediately. Stays inside the dialog so the
// operator can fine-tune from the reset baseline before clicking Save.
func (s *Server) settingsTaggerThresholdsResetPost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	catalog := catalogEntryByName(s.cfg.Paths.ModelPath, name)
	defaults := tagger.SeedTaggerInstance(name, false, catalog)
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = defaults.ConfidenceThreshold
		t.CategoryThresholds = defaults.CategoryThresholds
		t.PerCategoryTopK = defaults.PerCategoryTopK
		t.DisabledCategories = defaults.DisabledCategories
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q thresholds reset to defaults", name)
	rows, global, ok := s.thresholdDialogData(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	csrf := s.csrfToken(sessionFromContext(r.Context()))
	summary := taggerThresholdSummary(global, defaults.CategoryThresholds, defaults.DisabledCategories)
	_, _ = fmt.Fprintf(w, `<span id="tagger-thresh-summary-%s" hx-swap-oob="true">%s</span>`,
		html.EscapeString(name), html.EscapeString(summary))
	s.renderTemplate(w, "partials/tagger_thresholds_dialog.html", map[string]any{
		"Name":      name,
		"Global":    global,
		"Rows":      rows,
		"CSRFToken": csrf,
	})
}

// thresholdDialogData assembles the per-row state the template renders:
// one entry per category the profile is expected to emit, plus any
// extra categories carrying an existing override (so a dispatch-driven
// override stays editable). global is the live ConfidenceThreshold.
// ok=false means the tagger isn't in cfg or on disk.
func (s *Server) thresholdDialogData(name string) (rows []thresholdRow, global float64, ok bool) {
	s.cfgMu.Lock()
	var inst config.TaggerInstance
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			inst = t
			ok = true
			break
		}
	}
	modelPath := s.cfg.Paths.ModelPath
	s.cfgMu.Unlock()

	if !ok {
		// Fall back to the discovery default so a never-enabled row can
		// still open the dialog, seeding from the catalog when possible.
		for _, t := range tagger.DiscoverTaggers(s.cfg) {
			if t.Name == name {
				inst = t.TaggerInstance
				ok = true
				break
			}
		}
		if !ok {
			return nil, 0, false
		}
	}
	global = inst.ConfidenceThreshold

	tagsFile := inst.TagsFile
	if tagsFile == "" {
		tagsFile = tagger.DefaultTagsFile
	}
	profile, _ := tagger.ResolveProfile(modelPath, name, tagsFile)
	emit := profile.EmittedCategories()

	colors := s.categoryColors()

	// Catalog-seeded defaults drive the per-row Reset so it restores the
	// same per-category values the dialog-level "Reset to defaults" would
	// (see settingsTaggerThresholdsResetPost).
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(modelPath, name))

	seen := map[string]bool{}
	appendRow := func(cat string) {
		if seen[cat] {
			return
		}
		seen[cat] = true
		rows = append(rows, thresholdRow{
			Category:         cat,
			Override:         formatOverride(inst.CategoryThresholds, cat),
			MaxTags:          formatTopKOverride(inst.PerCategoryTopK, cat),
			MaxDefault:       tagger.ResolveTopK(nil, cat),
			Color:            colors[cat],
			Disabled:         slices.Contains(inst.DisabledCategories, cat),
			DefaultThreshold: formatOverride(defaults.CategoryThresholds, cat),
			DefaultMaxTags:   formatTopKOverride(defaults.PerCategoryTopK, cat),
		})
	}
	for _, cat := range emit {
		appendRow(cat)
	}
	// Categories the tagger only reaches through dispatch routing (e.g.
	// wd-swinv2 sends some general labels into medium / meta / year) are
	// emittable too, so surface them - restricted to categories that
	// exist on the gallery, since a rule pointing at a missing one is
	// skipped at inference time anyway.
	for _, cat := range tagger.DispatchTargetCategories(modelPath, name) {
		if _, ok := colors[cat]; ok {
			appendRow(cat)
		}
	}
	// Extra overrides (threshold, top-K, or disabled) not in the
	// profile's emitted set still render so the operator can edit /
	// clear them (dispatch rules can land a label in any category).
	for cat := range inst.CategoryThresholds {
		appendRow(cat)
	}
	for cat := range inst.PerCategoryTopK {
		appendRow(cat)
	}
	for _, cat := range inst.DisabledCategories {
		appendRow(cat)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Category < rows[j].Category })
	return rows, global, true
}

func formatOverride(m map[string]float64, key string) string {
	if v, ok := m[key]; ok {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return ""
}

// formatTopKOverride mirrors formatOverride for the per-category cap.
// A missing key returns "" so the input shows the placeholder; an
// explicit zero returns "0" so the operator's opt-out persists.
func formatTopKOverride(m map[string]int, key string) string {
	if v, ok := m[key]; ok {
		return strconv.Itoa(v)
	}
	return ""
}

// taggerThresholdSummary renders the inline summary the table cell
// shows next to the Configure button: "global 0.40" or "global 0.40,
// character 0.85, copyright 0.50". Disabled categories trail in a
// "(disabled: ...)" group so a muted category is visible without
// opening the dialog. Both lists are sorted by category name so two
// equivalent configs render the same string.
func taggerThresholdSummary(global float64, overrides map[string]float64, disabled []string) string {
	out := fmt.Sprintf("global %.2f", global)
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out += fmt.Sprintf(", %s %.2f", k, overrides[k])
	}
	if len(disabled) > 0 {
		d := append([]string(nil), disabled...)
		sort.Strings(d)
		out += " (disabled: " + strings.Join(d, ", ") + ")"
	}
	return out
}

// settingsTaggerGalleriesGet renders the dialog body for one tagger's
// per-gallery selection. One checkbox per configured gallery; the "all
// galleries" sentinel renders pre-checked when the TaggerInstance has
// no explicit Galleries list (the legacy default).
func (s *Server) settingsTaggerGalleriesGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	rows, allChecked, ok := s.galleryDialogData(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	csrf := s.csrfToken(sessionFromContext(r.Context()))
	s.renderTemplate(w, "partials/tagger_galleries_dialog.html", map[string]any{
		"Name":       name,
		"Rows":       rows,
		"AllChecked": allChecked,
		"CSRFToken":  csrf,
	})
}

// settingsTaggerGalleriesPost saves the per-tagger Galleries list.
// Three submitted shapes:
//   - `all=on`                       → nil (every gallery, legacy)
//   - `all=off` with selected names  → those names
//   - `all=off` with no selection    → []string{} (no gallery, dormant)
//
// The explicit-empty case is preserved by storing a non-nil empty slice
// so the TOML round-trip writes `galleries = []` and AppliesToGallery
// returns false everywhere on the next read.
//
// On success the dialog closes via the shared tagger-saved HX-Trigger.
func (s *Server) settingsTaggerGalleriesPost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	all := r.FormValue("all") == "on"
	var galleries []string
	if !all {
		// Filter against configured gallery names so a stale form value
		// can't poison the config (a renamed gallery would otherwise
		// linger here and silently disable the tagger). A non-nil empty
		// slice represents the explicit "no galleries" choice.
		galleries = []string{}
		valid := map[string]bool{}
		for _, g := range s.cfg.Galleries {
			valid[g.Name] = true
		}
		for _, n := range r.Form["gallery_names"] {
			n = strings.TrimSpace(n)
			if n == "" || !valid[n] {
				continue
			}
			galleries = append(galleries, n)
		}
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.Galleries = galleries
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q galleries updated (all=%t, %d named)", name, all, len(galleries))
	summary := taggerGallerySummary(galleries, len(s.cfg.Galleries))
	setTaggerSavedTrigger(w, "tagger-gal-"+name)
	_, _ = fmt.Fprintf(w,
		`<span id="tagger-gal-summary-%s" hx-swap-oob="true">%s</span>`+
			`<div id="flash-tagger" hx-swap-oob="true"><div class="flash flash-ok">Tagger %s galleries saved.</div></div>`,
		html.EscapeString(name), html.EscapeString(summary), html.EscapeString(name))
}

// galleryDialogData returns one row per configured gallery, with
// Checked reflecting the tagger's current Galleries list. allChecked
// is true when Galleries is nil (legacy "every gallery") so the
// master toggle renders pre-ticked. A non-nil empty slice means
// "no galleries", which surfaces as the master toggle off and every
// row unchecked.
func (s *Server) galleryDialogData(name string) (rows []taggerGalleryRow, allChecked bool, ok bool) {
	s.cfgMu.Lock()
	var inst config.TaggerInstance
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			inst = t
			ok = true
			break
		}
	}
	galleries := append([]config.Gallery(nil), s.cfg.Galleries...)
	s.cfgMu.Unlock()

	if !ok {
		for _, t := range tagger.DiscoverTaggers(s.cfg) {
			if t.Name == name {
				inst = t.TaggerInstance
				ok = true
				break
			}
		}
		if !ok {
			return nil, false, false
		}
	}
	allChecked = inst.Galleries == nil
	picked := map[string]bool{}
	for _, n := range inst.Galleries {
		picked[n] = true
	}
	for _, g := range galleries {
		rows = append(rows, taggerGalleryRow{
			Name:    g.Name,
			Checked: allChecked || picked[g.Name],
		})
	}
	return rows, allChecked, true
}

// taggerGallerySummary renders the per-row summary text shown next to
// the Galleries Configure button. nil reads as "(all)" - the legacy
// applies-everywhere default; explicit empty reads as "(none)" so the
// dormant case is distinguishable at a glance. Listing every configured
// gallery also reads "(all)" so picking every box produces the same
// short summary.
func taggerGallerySummary(galleries []string, totalGalleries int) string {
	if galleries == nil {
		return "(all)"
	}
	if len(galleries) == 0 {
		return "(none)"
	}
	if len(galleries) == totalGalleries {
		return "(all)"
	}
	return strings.Join(galleries, ", ")
}

// catalogEntryByName looks up a catalog row by name, returning nil for
// taggers that aren't in the catalog (homegrown subfolders). Used by
// the per-row Enable / Disable handlers to seed catalog-supplied
// thresholds onto fresh TaggerInstance rows.
func catalogEntryByName(modelPath, name string) *tagger.CatalogEntry {
	for _, e := range tagger.LoadCatalog(modelPath) {
		if e.Name == name {
			entry := e
			return &entry
		}
	}
	return nil
}

// disableUnavailableTaggers flips Enabled to false on any configured tagger
// whose model files have gone missing on disk. Persists the result so a
// re-downloaded model has to be re-enabled deliberately rather than firing
// off a half-broken job.
func (s *Server) disableUnavailableTaggers() {
	available := map[string]bool{}
	for _, t := range tagger.DiscoverTaggers(s.cfg) {
		available[t.Name] = t.Available
	}
	s.cfgMu.Lock()
	changed := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Enabled && !available[t.Name] {
			s.cfg.Tagger.Taggers[i].Enabled = false
			changed = true
			logx.Infof("settings: auto-disabled tagger %q (files missing)", t.Name)
		}
	}
	s.cfgMu.Unlock()
	if changed {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-disable taggers: save config: %v", err)
		}
	}
}

// persistNewlyDiscoveredTaggers materialises a TOML entry for any
// available subfolder under model_path that has no entry yet, with
// Enabled=true and the catalog-supplied threshold defaults applied.
// DiscoverTaggers already surfaces these rows as enabled at render
// time, but the state was implicit (derived on the fly each call);
// persisting it makes the intent visible in the config file and
// removes the chance of a future code path treating "no TOML entry"
// as "not enabled".
func (s *Server) persistNewlyDiscoveredTaggers() {
	discovered := tagger.DiscoverTaggers(s.cfg)
	modelPath := s.cfg.Paths.ModelPath
	s.cfgMu.Lock()
	known := make(map[string]bool, len(s.cfg.Tagger.Taggers))
	for _, t := range s.cfg.Tagger.Taggers {
		known[t.Name] = true
	}
	added := false
	for _, d := range discovered {
		if known[d.Name] || !d.Available {
			continue
		}
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers,
			tagger.SeedTaggerInstance(d.Name, true, catalogEntryByName(modelPath, d.Name)))
		known[d.Name] = true
		added = true
		logx.Infof("settings: auto-enabled discovered tagger %q", d.Name)
	}
	s.cfgMu.Unlock()
	if added {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-enable taggers: save config: %v", err)
		}
	}
}

// settingsTaggerDeletePost removes a tagger entry from the config and wipes
// its subfolder under paths.model_path. Refused if the tagger is currently
// enabled (the UI hides the button in that case; this is the server gate).
// The name is validated so it can't escape model_path with `..` segments.
func (s *Server) settingsTaggerDeletePost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r, "name")
	if !ok {
		return
	}
	s.cfgMu.Lock()
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name && t.Enabled {
			s.cfgMu.Unlock()
			writeInlineFlash(w, "err", "Disable tagger "+name+" before deleting it.")
			return
		}
	}
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers[:i], s.cfg.Tagger.Taggers[i+1:]...)
			break
		}
	}
	dir := filepath.Join(s.cfg.Paths.ModelPath, name)
	s.cfgMu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		logx.Warnf("delete tagger %q: remove %q: %v", name, dir, err)
		writeInlineFlash(w, "err", "Removed config entry but could not delete folder: "+err.Error())
		return
	}
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q deleted (folder %s removed)", name, dir)
	w.Header().Set("HX-Refresh", "true")
	writeInlineFlash(w, "ok", "Tagger "+name+" deleted.")
}
