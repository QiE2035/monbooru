package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/i18n"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tagger"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "settings", "Settings - "+s.booruName())
	s.disableUnavailableTaggers()
	s.persistNewlyDiscoveredTaggers()
	taggers := tagger.AvailableTaggers(s.cfg)
	// Build a unified row list: catalog-backed rows (installed-and-in-catalog
	// plus catalog entries whose subfolder isn't on disk yet) come first as
	// "supported"; user-only installed taggers (not in the catalog) come last
	// as "unsupported". The template renders a separator between the two
	// groups when both are non-empty.
	modelPath := s.cfg.Paths.ModelPath
	catalog := tagger.LoadCatalog(modelPath)
	taggerByName := map[string]tagger.TaggerStatus{}
	for _, t := range taggers {
		taggerByName[t.Name] = t
	}
	// Supported rows track the catalog order (wd-swinv2 → joytag →
	// camie-v2 by default) so the table reflects the editorial
	// recommendation, not the alphabetical disk-readdir order. For each
	// catalog entry, surface the installed row when present, otherwise
	// the ghost row.
	var supportedRows, unsupportedRows []taggerRow
	totalGalleries := len(s.cfg.Galleries)
	catalogNames := map[string]bool{}
	for _, e := range catalog {
		catalogNames[e.Name] = true
		if t, installed := taggerByName[e.Name]; installed {
			row := installedTaggerRow(t, totalGalleries)
			row.Supported = true
			row.Description = e.Description
			row.HostCommand = e.HostCommand()
			row.DockerCommand = e.DockerCommand("monbooru")
			supportedRows = append(supportedRows, row)
		} else {
			supportedRows = append(supportedRows, taggerRow{
				Name:          e.Name,
				Description:   e.Description,
				Supported:     true,
				HostCommand:   e.HostCommand(),
				DockerCommand: e.DockerCommand("monbooru"),
			})
		}
	}
	// User-installed taggers that aren't in the catalog land below the
	// supported set in their disk-discovery order.
	for _, t := range taggers {
		if catalogNames[t.Name] {
			continue
		}
		unsupportedRows = append(unsupportedRows, installedTaggerRow(t, totalGalleries))
	}
	taggerRows := append(supportedRows, unsupportedRows...)
	data := base.AsMap()
	data["Galleries"] = s.galleryRowsWithSnapshot(s.activeName, base.VisibleCount, base.TagCount)
	data["Config"] = s.cfg
	data["Taggers"] = taggers
	data["TaggerRows"] = taggerRows
	data["SupportedCount"] = len(supportedRows)
	data["UnsupportedCount"] = len(unsupportedRows)
	data["ScheduleStatus"] = s.ScheduleStatus()
	data["Stats"] = s.gatherStats()
	data["MonloaderPending"] = s.pairs.listPending()
	data["MonloaderPaired"] = s.pairedWith("monloader")
	data["MonloaderPeerURL"] = s.monloaderAPIBase()
	// AvailableLangs drives the Language dropdown in the i18n section.
	// Read the embed FS at render time so a future translator who adds a
	// new .toml under internal/i18n/locales/ gets it for free without a
	// server restart - well, almost: the bundle itself is process-pinned
	// to what MustInit saw, but the dropdown at least reflects every
	// translatable locale the binary shipped with. If the embed read
	// fails (a torn build, an FS shim gone wrong) we fall back to the
	// current language so the section still renders one option.
	avail, err := i18n.AvailableLocales()
	if err != nil || len(avail) == 0 {
		avail = []string{s.cfg.I18n.Language}
	}
	data["AvailableLangs"] = avail
	s.renderTemplate(w, "settings.html", data)
}

func (s *Server) settingsSchedulePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	timeVal := strings.TrimSpace(r.FormValue("time"))
	if timeVal == "" {
		timeVal = "01:00"
	}
	if err := config.ValidateScheduleTime(timeVal); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	s.cfgMu.Lock()
	s.cfg.Schedule.Time = timeVal
	s.cfg.Schedule.SyncGallery = r.FormValue("sync_gallery") == "on"
	s.cfg.Schedule.RemoveOrphans = r.FormValue("remove_orphans") == "on"
	s.cfg.Schedule.RunAutoTaggers = r.FormValue("run_auto_taggers") == "on"
	s.cfg.Schedule.FindRelationPairs = r.FormValue("find_relation_pairs") == "on"
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	select {
	case s.schedReload <- struct{}{}:
	default:
	}
	logx.Infof("settings: schedule updated (time=%s)", timeVal)
	writeInlineFlash(w, "ok", "Saved.")
}

// settingsGeneralPost saves the unified Settings → General form: the Files
// subsection (watch toggle + max file size) and the UI subsection (page
// size + thumbnail fit). One submit covers both so the page has a single
// Save button.
func (s *Server) settingsGeneralPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	uploadFolder := strings.TrimSpace(r.FormValue("default_upload_folder"))
	if _, err := gallery.ResolveSubdir(s.galleryPath(), uploadFolder); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	s.cfgMu.Lock()
	s.cfg.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
	if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil && n >= 0 {
		s.cfg.Gallery.MaxFileSizeMB = n
	}
	s.cfg.Gallery.DefaultUploadFolder = uploadFolder
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
		s.cfg.UI.PageSize = min(n, config.MaxPageSize)
	}
	if fit := r.FormValue("thumbnail_fit"); fit == "square" || fit == "natural" {
		s.cfg.UI.ThumbnailFit = fit
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: general updated")
	writeInlineFlash(w, "ok", "Saved.")
}

// settingsLanguagePost updates the [i18n] language field and writes it
// back to monbooru.toml, then calls i18n.SetLanguage to swap the
// process-wide localizer so the new language takes effect immediately
// without a restart.
func (s *Server) settingsLanguagePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	lang := strings.TrimSpace(r.FormValue("language"))
	if lang == "" {
		writeInlineFlash(w, "err", "language must not be empty")
		return
	}
	// Refuse a code that has no matching locale file: writing a value
	// the binary can't resolve would make the next start fail with the
	// very error the operator is trying to fix from the settings page.
	avail, err := i18n.AvailableLocales()
	if err != nil {
		writeInlineFlash(w, "err", "could not list languages: "+err.Error())
		return
	}
	if !slices.Contains(avail, lang) {
		writeInlineFlash(w, "err", fmt.Sprintf("language %q is not available", lang))
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.I18n.Language = lang
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if err := i18n.SetLanguage(lang); err != nil {
		writeInlineFlash(w, "err", "Could not switch language: "+err.Error())
		return
	}
	logx.Infof("settings: language set to %q", lang)
	writeInlineFlash(w, "ok", "Language saved.")
}

func (s *Server) settingsMonloaderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Server.MonloaderURL = strings.TrimSpace(r.FormValue("monloader_url"))
	s.cfg.Monloader.APIURL = strings.TrimSpace(r.FormValue("api_url"))
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: monloader link updated")
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	currentPass := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	if newPass == "" {
		writeInlineFlash(w, "err", "New password required.")
		return
	}
	// If a password is already set, require the current one for verification.
	if s.cfg.Auth.EnablePassword && s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		writeInlineFlash(w, "err", "Error hashing password.")
		return
	}
	s.cfgMu.Lock()
	s.cfg.Auth.PasswordHash = string(hash)
	s.cfg.Auth.EnablePassword = true
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password updated from %s", clientIP(r))
	writeInlineFlash(w, "ok", "Password updated.")
	s.renderAuthPasswordOOB(w, r)
}

func (s *Server) settingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := config.ValidateTokenName(name); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	tok, secret := config.GenerateToken(name, config.AllScopes)
	if err := s.withConfig(func(c *config.Config) error {
		if c.TokenNameExists(name) {
			return fmt.Errorf("a token named %q already exists", name)
		}
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	logx.Infof("settings: API token %q created from %s", name, clientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, "partials/flash_token.html", map[string]any{"Token": secret})
	s.renderAuthTokensOOB(w, r)
	_, _ = w.Write([]byte(`<script>(function(){var i=document.getElementById('token-name-input');if(i)i.value='';})();</script>`))
}

// tokenPaired reports whether the token with the given id is managed by a
// pairing, and so must be changed through the pairing rather than directly.
func (s *Server) tokenPaired(id string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	for _, t := range s.cfg.Auth.Tokens {
		if t.ID == id {
			return t.Paired != ""
		}
	}
	return false
}

func (s *Server) settingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.tokenPaired(id) {
		writeInlineFlash(w, "err", "This token is managed by a pairing; remove the pairing instead.")
		return
	}
	var removed bool
	if err := s.withConfig(func(c *config.Config) error {
		removed = c.RemoveToken(id)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !removed {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s revoked from %s", id, clientIP(r))
	writeInlineFlash(w, "ok", "Token revoked.")
	s.renderAuthTokensOOB(w, r)
}

// renderAuthTokensOOB writes an out-of-band swap of the API-token list so it
// reflects the latest set without a page reload.
func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	tokens := slices.Clone(s.cfg.Auth.Tokens)
	s.cfgMu.Unlock()
	s.renderTemplate(w, "partials/auth_tokens.html", map[string]any{
		"Tokens":    tokens,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"OOB":       true,
	})
}

type tokenScopeRow struct {
	Name    string
	Desc    string
	Checked bool
}

func (s *Server) settingsTokenPrivilegesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cfgMu.Lock()
	var scopes []string
	var found, paired bool
	for _, t := range s.cfg.Auth.Tokens {
		if t.ID == id {
			scopes = slices.Clone(t.Scopes)
			paired = t.Paired != ""
			found = true
			break
		}
	}
	s.cfgMu.Unlock()
	if !found {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	descs := map[string]string{
		config.ScopeRead:   "read - all GET endpoints",
		config.ScopeWrite:  "write - create and modify",
		config.ScopeDelete: "delete - destructive actions",
	}
	rows := make([]tokenScopeRow, 0, len(config.AllScopes))
	for _, sc := range config.AllScopes {
		rows = append(rows, tokenScopeRow{Name: sc, Desc: descs[sc], Checked: slices.Contains(scopes, sc)})
	}
	s.renderTemplate(w, "partials/token_privileges_dialog.html", map[string]any{
		"ID":        id,
		"Scopes":    rows,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"Paired":    paired,
	})
}

func (s *Server) settingsTokenPrivilegesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.tokenPaired(id) {
		writeInlineFlash(w, "err", "This token is managed by a pairing; its privileges can't be changed.")
		return
	}
	scopes := filterScopes(r.Form["scope"])
	var found bool
	if err := s.withConfig(func(c *config.Config) error {
		found = c.SetTokenScopes(id, scopes)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !found {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s privileges updated from %s", id, clientIP(r))
	setDialogSavedTrigger(w, "token-saved", "token-cfg-"+id)
	writeOOBSummaryFlash(w, "token-scopes-"+id, strings.Join(scopes, " "), "flash-auth", localize("flash.token_privileges_saved"))
}

// filterScopes keeps only recognized scopes, in canonical order, dropping
// anything a tampered form might submit.
func filterScopes(in []string) []string {
	var out []string
	for _, sc := range config.AllScopes {
		if slices.Contains(in, sc) {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	// Require current password whenever a hash is set, even if
	// EnablePassword has been flipped off in TOML. Mirrors the password-
	// change handler so the disable path can't be bypassed by editing
	// the file in place and then visiting /settings/auth/password/remove.
	currentPass := r.FormValue("current_password")
	if s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	s.cfgMu.Lock()
	s.cfg.Auth.EnablePassword = false
	s.cfg.Auth.PasswordHash = ""
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password removed from %s", clientIP(r))
	// Invalidate all sessions so nobody is locked out of the now-open instance
	s.sessions.Clear()
	writeInlineFlash(w, "ok", "Password removed. Authentication is now disabled.")
	s.renderAuthPasswordOOB(w, r)
}
