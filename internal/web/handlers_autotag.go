package web

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// uploadPage renders the multi-file upload form.
func (s *Server) uploadPage(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "upload", "Upload - Monbooru")
	s.renderTemplate(w, "upload.html", map[string]any{
		"Title":           base.Title,
		"ActiveNav":       base.ActiveNav,
		"CSRFToken":       base.CSRFToken,
		"AuthEnabled":     base.AuthEnabled,
		"Degraded":        base.Degraded,
		"Version":         base.Version,
		"RepoURL":         base.RepoURL,
		"Variant":         base.Variant,
		"CustomCSS":       base.CustomCSS,
		"ActiveGallery":   base.ActiveGallery,
		"Galleries":       s.galleryList(),
		"VisibleCount":    base.VisibleCount,
		"TagCount":        base.TagCount,
		"SavedCount":      base.SavedCount,
		"RatingLevels":    base.RatingLevels,
		"ActiveRating":    base.ActiveRating,
		"AcceptFileTypes": gallery.SupportedMIMETypes,
		"TaggerAvailable": tagger.IsAvailable(s.cfg),
		"EnabledTaggers":  tagger.EnabledTaggers(s.cfg),
	})
}

// uploadPost handles the multi-file form submit. Per-file size, tagging and
// optional autotag-after-upload all flow through here.
func (s *Server) uploadPost(w http.ResponseWriter, r *http.Request) {
	if cx := s.Active(); cx == nil || cx.Degraded {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`<div class="flash flash-err">Upload unavailable: gallery path is unreadable.</div>`))
		return
	}
	maxBytes := int64(s.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024
	// MaxFileSizeMB <= 0 disables the per-file cap (Sync and the watcher
	// treat it the same way); skip MaxBytesReader entirely so a single
	// 4 KiB total-body cap doesn't make every upload fail.
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes*10+4096) // allow multiple files
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		w.Write([]byte(`<div class="flash flash-err">Upload too large or invalid.</div>`))
		return
	}

	tagInput := strings.TrimSpace(r.FormValue("tags"))
	autotagAfter := r.FormValue("autotag") == "on"
	folderInput := strings.TrimSpace(r.FormValue("folder"))
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		w.Write([]byte(`<div class="flash flash-err">No files selected.</div>`))
		return
	}

	destDir, destErr := gallery.ResolveSubdir(s.galleryPath(), folderInput)
	if destErr != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(destErr.Error()) + `</div>`))
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		w.Write([]byte(`<div class="flash flash-err">Could not create folder: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	// Resolve tags using the shared parser (same logic as addTagToImage).
	var tagPairs []catTag
	if tagInput != "" {
		tagPairs, _ = s.parseTagInput(tagInput)
	}

	var addedIDs []int64
	var tagWarnings []string
	added, dupes, errors, oversized := 0, 0, 0, 0
	for _, fh := range files {
		// Enforce the per-file cap up front; the watcher and API handler do the
		// same. The MaxBytesReader cap above only bounds the total request body,
		// so without this a single multi-GB file inside a multipart upload
		// would still slip through and stall thumbnail generation.
		if maxBytes > 0 && fh.Size > maxBytes {
			oversized++
			continue
		}
		file, err := fh.Open()
		if err != nil {
			errors++
			continue
		}

		dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
		dst, err := os.Create(dstPath)
		if err != nil {
			file.Close()
			errors++
			continue
		}

		if _, err := dst.ReadFrom(file); err != nil {
			dst.Close()
			file.Close()
			os.Remove(dstPath)
			errors++
			continue
		}
		dst.Close()
		file.Close()

		ft, ftErr := gallery.DetectFileType(dstPath)
		if ftErr != nil {
			os.Remove(dstPath)
			errors++
			continue
		}

		img, isDup, ingestErr := gallery.Ingest(s.db(), s.galleryPath(), s.thumbnailsPath(), dstPath, ft, models.OriginUpload)
		if ingestErr != nil {
			logx.Warnf("upload ingest %q: %v", fh.Filename, ingestErr)
			errors++
			continue
		}
		if isDup {
			dupes++
			continue
		}

		for _, ct := range tagPairs {
			tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				tagWarnings = append(tagWarnings, ct.name+": "+err.Error())
				continue
			}
			if err := s.tagSvc().AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
				tagWarnings = append(tagWarnings, ct.name+": "+err.Error())
			}
		}
		addedIDs = append(addedIDs, img.ID)
		added++
	}

	if added > 0 {
		s.Active().InvalidateCaches()
	}

	msg := fmt.Sprintf("%d added", added)
	if dupes > 0 {
		msg += fmt.Sprintf(", %d duplicate(s)", dupes)
	}
	if oversized > 0 {
		msg += fmt.Sprintf(", %d skipped (exceeds %d MB)", oversized, s.cfg.Gallery.MaxFileSizeMB)
	}
	if errors > 0 {
		msg += fmt.Sprintf(", %d error(s)", errors)
	}
	if len(tagWarnings) > 0 {
		msg += fmt.Sprintf(" (%d tag warning(s): %s)", len(tagWarnings), strings.Join(tagWarnings, "; "))
	}
	cssClass := "flash-ok"
	if added == 0 && (errors > 0 || oversized > 0) {
		cssClass = "flash-err"
	}

	// Optionally kick off auto-tagging on the newly uploaded images.
	if autotagAfter && len(addedIDs) > 0 && tagger.IsAvailable(s.cfg) {
		selected, selErr := selectTaggers(s.cfg, taggerName)
		if selErr != nil {
			msg += " (autotag skipped: " + selErr.Error() + ")"
		} else if err := s.jobs.Start("autotag"); err != nil {
			msg += " (autotag skipped: a job is already running)"
		} else {
			ids := addedIDs
			database := s.db()
			cx := s.Active()
			go func() {
				ctx := s.jobs.Context()
				skipped, err := tagger.RunWithTaggers(ctx, database, s.cfg, ids, selected, s.jobs, s.cfg.Tagger.UseCUDA)
				cx.InvalidateCaches()
				if ctx.Err() != nil {
					s.jobs.Complete(fmt.Sprintf("auto-tagging cancelled (%d image(s) queued)", len(ids)))
					return
				}
				if err != nil {
					s.jobs.Fail(err.Error())
					return
				}
				if skipped > 0 {
					s.jobs.Complete(fmt.Sprintf("auto-tagged %d of %d uploaded image(s), %d skipped", len(ids)-skipped, len(ids), skipped))
					return
				}
				s.jobs.Complete(fmt.Sprintf("auto-tagged %d uploaded image(s)", len(ids)))
			}()
			msg += fmt.Sprintf(", auto-tagging %d image(s)", len(addedIDs))
		}
	}
	w.Write([]byte(`<div class="flash ` + cssClass + `">` + html.EscapeString(msg) + `</div>`))
}

func (s *Server) autotagTrigger(w http.ResponseWriter, r *http.Request) {
	if !tagger.IsAvailable(s.cfg) {
		http.Error(w, "auto-tagger not available: "+tagger.UnavailableReason(s.cfg), http.StatusServiceUnavailable)
		return
	}

	if !parseFormOK(w, r) {
		return
	}
	scope := strings.TrimSpace(r.FormValue("scope"))
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := selectTaggers(s.cfg, taggerName)
	if selErr != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(selErr.Error()) + `</div>`))
			return
		}
		http.Error(w, selErr.Error(), http.StatusBadRequest)
		return
	}

	var ids []int64
	if scope == "search" {
		// Mirror batchTag's search-side materialisation: parse q, stream
		// matching ids off ExecuteForDeleteStream so the cursor walks the
		// result set without buffering an extra copy.
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			if isHTMXRequest(r) {
				w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
					html.EscapeString(parseErr.Error()) + `</div>`))
				return
			}
			http.Error(w, parseErr.Error(), http.StatusBadRequest)
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("autotag search: %v", err)
			if isHTMXRequest(r) {
				w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
				return
			}
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
	} else {
		// scope=selection or empty: read the checked ids from the form.
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	}

	if len(ids) == 0 {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">No images to tag.</div>`))
			return
		}
		http.Error(w, "no images selected", http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start("autotag"); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
			return
		}
		http.Error(w, "job already running", http.StatusConflict)
		return
	}
	// Loading the ONNX model (and initialising CUDA when enabled) can take a
	// few seconds before the first image completes; surface that up front so
	// the status bar doesn't look stalled.
	s.jobs.Update(0, len(ids), "starting (loading model may take a few seconds)…")

	database := s.db()
	cx := s.Active()
	go func() {
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, s.cfg, ids, selected, s.jobs, s.cfg.Tagger.UseCUDA)
		// New tags are commonly created by a tagger run, so the cached
		// tag count is stale once the worker returns regardless of
		// outcome (cancelled runs still wrote rows for completed images).
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete(fmt.Sprintf("auto-tagging cancelled (%d image(s) queued)", len(ids)))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if skipped > 0 {
			s.jobs.Complete(fmt.Sprintf("auto-tagged %d of %d image(s), %d skipped", len(ids)-skipped, len(ids), skipped))
			return
		}
		s.jobs.Complete(fmt.Sprintf("auto-tagged %d image(s)", len(ids)))
	}()

	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Auto-tagger started for ` + fmt.Sprintf("%d", len(ids)) + ` image(s).</div>`))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) autotagImage(w http.ResponseWriter, r *http.Request) {
	if !tagger.IsAvailable(s.cfg) {
		reason := tagger.UnavailableReason(s.cfg)
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Auto-tagger not available: ` + html.EscapeString(reason) + `.</div>`))
			return
		}
		http.Error(w, "auto-tagger not available: "+reason, http.StatusServiceUnavailable)
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := selectTaggers(s.cfg, taggerName)
	if selErr != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(selErr.Error()) + `</div>`))
			return
		}
		http.Error(w, selErr.Error(), http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start("autotag"); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
			return
		}
		http.Error(w, "job already running", http.StatusConflict)
		return
	}

	database := s.db()
	cx := s.Active()
	go func() {
		// Force CPU inference for one-shot detail-page runs: spinning up the
		// CUDA session and loading the model onto the GPU dwarfs the tagging
		// time for a single image, so CPU finishes faster even when the
		// global toggle is on.
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, s.cfg, []int64{id}, selected, s.jobs, false)
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete("auto-tagging cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if skipped > 0 {
			s.jobs.Complete("auto-tagger skipped image #" + idStr)
			return
		}
		s.jobs.Complete("auto-tagged image #" + idStr)
	}()

	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Auto-tagger started for this image.</div>`))
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// selectTaggers resolves a user-supplied tagger_name to the concrete
// TaggerStatus list to run. Empty name means all enabled+available taggers.
// Returns an error if the requested tagger is not enabled or unavailable.
func selectTaggers(cfg *config.Config, name string) ([]tagger.TaggerStatus, error) {
	enabled := tagger.EnabledTaggers(cfg)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []tagger.TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available", name)
}
