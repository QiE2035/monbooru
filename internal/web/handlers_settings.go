package web

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
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
			supportedRows = append(supportedRows, taggerRow{
				Name:                t.Name,
				Available:           t.Available,
				Reason:              t.Reason,
				Enabled:             t.Enabled,
				ConfidenceThreshold: t.ConfidenceThreshold,
				ThresholdSummary:    taggerThresholdSummary(t.ConfidenceThreshold, t.CategoryThresholds),
				GallerySummary:      taggerGallerySummary(t.Galleries, totalGalleries),
				Installed:           true,
				Supported:           true,
				Description:         e.Description,
				HostCommand:         e.HostCommand(),
				DockerCommand:       e.DockerCommand("monbooru"),
			})
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
		unsupportedRows = append(unsupportedRows, taggerRow{
			Name:                t.Name,
			Available:           t.Available,
			Reason:              t.Reason,
			Enabled:             t.Enabled,
			ConfidenceThreshold: t.ConfidenceThreshold,
			ThresholdSummary:    taggerThresholdSummary(t.ConfidenceThreshold, t.CategoryThresholds),
			GallerySummary:      taggerGallerySummary(t.Galleries, totalGalleries),
			Installed:           true,
		})
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
// size). One submit covers both so the page has a single Save button.
func (s *Server) settingsGeneralPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
	if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil && n >= 0 {
		s.cfg.Gallery.MaxFileSizeMB = n
	}
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
		s.cfg.UI.PageSize = n
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: general updated")
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

func (s *Server) settingsTokenPost(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		logx.Errorf("generating API token: %v", err)
		writeInlineFlash(w, "err", "Failed to generate token.")
		return
	}
	token := fmt.Sprintf("%x", buf)
	s.cfgMu.Lock()
	s.cfg.Auth.APIToken = token
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: API token regenerated from %s", clientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, "partials/flash_token.html", map[string]any{"Token": token})
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
