package web

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// autotagSearchScopeCap bounds the scope=search materialisation so a
// clean-sweep autotag against an unbounded result set can't fill RAM
// with the ids slice plus the matching per-image frame state
// tagger.RunWithTaggers builds. Operators with a larger working set
// re-run the autotag job over narrower searches.
const autotagSearchScopeCap = 50000

// errAutotagOverCap is the sentinel the ExecuteForDeleteStream callback
// returns once autotagSearchScopeCap is reached, so the caller can
// distinguish "over cap" from a real cursor error.
var errAutotagOverCap = errors.New("autotag: search-scope cap reached")

// spawnAutoTagJob runs RunWithTaggers in a goroutine, flushes per-DB
// caches, and posts the completion summary. itemNoun ("" or
// "uploaded ") splices into the success / partial summaries.
func (s *Server) spawnAutoTagJob(ids []int64, selected []tagger.TaggerStatus, logScope, itemNoun string) {
	database := s.db()
	cx := s.Active()
	baseline := readVmRSS()
	go func() {
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, s.cfg, ids, selected, s.jobs, s.cfg.Tagger.UseCUDA, cx.MangaCacheDir())
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
		logAutotagPeak(fmt.Sprintf("%s %d image(s)", logScope, len(ids)), baseline)
		if skipped > 0 {
			s.jobs.Complete(fmt.Sprintf("auto-tagged %d of %d %simage(s), %d skipped", len(ids)-skipped, len(ids), itemNoun, skipped))
			return
		}
		s.jobs.Complete(fmt.Sprintf("auto-tagged %d %simage(s)", len(ids), itemNoun))
	}()
}

// logAutotagPeak writes the peak-RSS-delta for a finished autotag run
// at INFO level. baselineRSS is sampled before the run; post-run we
// read VmHWM (the kernel's RSS high-water mark) and subtract. No-op
// when the sample is missing or no peak over baseline is observed.
// scope identifies the run (e.g. the gallery name, image id, batch
// size) so operators reading logs can match deltas to the job that
// caused them.
func logAutotagPeak(scope string, baselineRSS uint64) {
	if baselineRSS == 0 {
		return
	}
	peak := readVmHWM()
	if peak <= baselineRSS {
		return
	}
	logx.Infof("autotag %s: peak RSS +%s", scope, humanBytesFmt(int64(peak-baselineRSS)))
}

// uploadPost handles the multi-file form submit. Per-file size, tagging and
// optional autotag-after-upload all flow through here.
func (s *Server) uploadPost(w http.ResponseWriter, r *http.Request) {
	if cx := s.Active(); cx == nil || cx.Degraded {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeInlineFlash(w, "err", "Upload unavailable: gallery path is unreadable.")
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
		writeInlineFlash(w, "err", "Upload too large or invalid.")
		return
	}

	tagInput := strings.TrimSpace(r.FormValue("tags"))
	autotagAfter := r.FormValue("autotag") == "on"
	folderInput := strings.TrimSpace(r.FormValue("folder"))
	// The inline inbox drop zone posts no folder field, so fall back to the
	// operator's configured default; an explicit folder still wins.
	if folderInput == "" {
		folderInput = strings.TrimSpace(s.cfg.Gallery.DefaultUploadFolder)
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeInlineFlash(w, "err", "No files selected.")
		return
	}

	destDir, destErr := gallery.ResolveSubdir(s.galleryPath(), folderInput)
	if destErr != nil {
		writeInlineFlash(w, "err", destErr.Error())
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		writeInlineFlash(w, "err", "Could not create folder: "+err.Error())
		return
	}

	// Resolve tags using the shared parser (same logic as addTagToImage).
	var tagPairs []catTag
	if tagInput != "" {
		tagPairs, _ = s.parseTagInput(tagInput)
	}

	var addedIDs []int64
	var dupeIDs []int64
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
			_ = file.Close()
			errors++
			continue
		}

		if _, err := dst.ReadFrom(file); err != nil {
			_ = dst.Close()
			_ = file.Close()
			_ = os.Remove(dstPath)
			errors++
			continue
		}
		_ = dst.Close()
		_ = file.Close()

		ft, ftErr := gallery.DetectFileType(dstPath)
		if ftErr != nil {
			_ = os.Remove(dstPath)
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
			dupeIDs = append(dupeIDs, img.ID)
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
		// Stamp every row from this POST with one token so the inbox cluster
		// view groups the whole drop together regardless of the time-gap rule.
		batch := time.Now().UnixNano()
		if err := db.Chunked(addedIDs, 500, func(chunk []int64) error {
			placeholders, args := db.InPlaceholders(chunk)
			_, execErr := s.db().Write.Exec(
				`UPDATE images SET upload_batch = ? WHERE id IN (`+placeholders+`)`,
				append([]any{batch}, args...)...)
			return execErr
		}); err != nil {
			logx.Warnf("upload: stamp batch token: %v", err)
		}
		s.Active().InvalidateCaches()
	}

	// The flash carries links to the duplicate rows, so it is assembled
	// as HTML; the int counts and ids are safe, and the operator/file
	// supplied substrings (tag warnings, tagger-selection error) are
	// escaped before they go in.
	var msg strings.Builder
	fmt.Fprintf(&msg, "%d added", added)
	if dupes > 0 {
		fmt.Fprintf(&msg, ", %d duplicate(s)", dupes)
		if len(dupeIDs) > 0 {
			msg.WriteString(" (")
			for i, id := range dupeIDs {
				if i > 0 {
					msg.WriteString(", ")
				}
				fmt.Fprintf(&msg, `<a href="/images/%d">#%d</a>`, id, id)
			}
			msg.WriteString(")")
		}
	}
	if oversized > 0 {
		fmt.Fprintf(&msg, ", %d skipped (exceeds %d MB)", oversized, s.cfg.Gallery.MaxFileSizeMB)
	}
	if errors > 0 {
		fmt.Fprintf(&msg, ", %d error(s)", errors)
	}
	if len(tagWarnings) > 0 {
		fmt.Fprintf(&msg, " (%d tag warning(s): %s)", len(tagWarnings), html.EscapeString(strings.Join(tagWarnings, "; ")))
	}
	cssClass := "flash-ok"
	if added == 0 && (errors > 0 || oversized > 0) {
		cssClass = "flash-err"
	}

	// Optionally kick off auto-tagging on the newly uploaded images.
	if autotagAfter && len(addedIDs) > 0 && tagger.IsAvailable(s.cfg) {
		selected, selErr := selectTaggers(s.cfg, s.activeName, taggerName)
		if selErr != nil {
			fmt.Fprintf(&msg, " (autotag skipped: %s)", html.EscapeString(selErr.Error()))
		} else if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
			msg.WriteString(" (autotag skipped: a job is already running)")
		} else {
			s.spawnAutoTagJob(addedIDs, selected, "upload", "uploaded ")
			fmt.Fprintf(&msg, ", auto-tagging %d image(s)", len(addedIDs))
		}
	}
	kind := "ok"
	if cssClass == "flash-err" {
		kind = "err"
	}
	writeInlineFlashHTML(w, kind, msg.String())
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

	selected, selErr := selectTaggers(s.cfg, s.activeName, taggerName)
	if selErr != nil {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", selErr.Error())
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
				writeInlineFlash(w, "err", "Could not parse search: "+parseErr.Error())
				return
			}
			http.Error(w, parseErr.Error(), http.StatusBadRequest)
			return
		}
		expr = resolveCeiling(r, s.Active()).Apply(expr)
		// Hard ceiling so a clean-sweep autotag against an unbounded
		// search doesn't materialise million-id slices plus the
		// matching per-image frame-extraction state in tagger.RunWithTaggers.
		// errAutotagOverCap stops the stream cleanly and surfaces a
		// "narrow your search" flash to the operator.
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			if len(ids) >= autotagSearchScopeCap {
				return errAutotagOverCap
			}
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil && err != errAutotagOverCap {
			logx.Errorf("autotag search: %v", err)
			if isHTMXRequest(r) {
				writeInlineFlash(w, "err", "Search error.")
				return
			}
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		if err == errAutotagOverCap {
			msg := fmt.Sprintf("Search matches more than %d images; narrow the query and re-run.", autotagSearchScopeCap)
			if isHTMXRequest(r) {
				writeInlineFlash(w, "err", msg)
				return
			}
			http.Error(w, msg, http.StatusBadRequest)
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
			writeInlineFlash(w, "err", "No images to tag.")
			return
		}
		http.Error(w, "no images selected", http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", "A job is already running.")
			return
		}
		http.Error(w, "job already running", http.StatusConflict)
		return
	}
	// Loading the ONNX model (and initialising CUDA when enabled) can take a
	// few seconds before the first image completes; surface that up front so
	// the status bar doesn't look stalled.
	s.jobs.Update(0, len(ids), "starting (loading model may take a few seconds)…")

	s.spawnAutoTagJob(ids, selected, "batch", "")

	if isHTMXRequest(r) {
		setFlashHeader(w, fmt.Sprintf("Auto-tagger started for %d image(s).", len(ids)), "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) autotagImage(w http.ResponseWriter, r *http.Request) {
	if !tagger.IsAvailable(s.cfg) {
		reason := tagger.UnavailableReason(s.cfg)
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", "Auto-tagger not available: "+reason+".")
			return
		}
		http.Error(w, "auto-tagger not available: "+reason, http.StatusServiceUnavailable)
		return
	}

	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))

	selected, selErr := selectTaggers(s.cfg, s.activeName, taggerName)
	if selErr != nil {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", selErr.Error())
			return
		}
		http.Error(w, selErr.Error(), http.StatusBadRequest)
		return
	}

	if err := s.jobs.Start(models.JobTypeAutotag); err != nil {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", "A job is already running.")
			return
		}
		http.Error(w, "job already running", http.StatusConflict)
		return
	}
	// Surface a starting line so the status bar isn't blank while the
	// model loads. Mirrors the batch-trigger handler's preamble.
	s.jobs.Update(0, 1, "starting (loading model may take a few seconds)…")

	database := s.db()
	cx := s.Active()
	baseline := readVmRSS()
	go func() {
		// Force CPU inference for one-shot detail-page runs: spinning up the
		// CUDA session and loading the model onto the GPU dwarfs the tagging
		// time for a single image, so CPU finishes faster even when the
		// global toggle is on.
		ctx := s.jobs.Context()
		skipped, err := tagger.RunWithTaggers(ctx, database, s.cfg, []int64{id}, selected, s.jobs, false, cx.MangaCacheDir())
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete("auto-tagging cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		logAutotagPeak(fmt.Sprintf("image #%d", id), baseline)
		if skipped > 0 {
			s.jobs.Complete(fmt.Sprintf("auto-tagger skipped image #%d", id))
			return
		}
		s.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", id))
	}()

	if isHTMXRequest(r) {
		setFlashHeader(w, "Auto-tagger started for this image.", "ok", nil)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// selectTaggers resolves a user-supplied tagger_name to the concrete
// TaggerStatus list to run on the named gallery. Empty name means
// every tagger enabled + available + applicable to that gallery.
// Returns an error if the requested tagger is not enabled, unavailable,
// or restricted to a different gallery.
func selectTaggers(cfg *config.Config, gallery, name string) ([]tagger.TaggerStatus, error) {
	enabled := tagger.EnabledTaggersForGallery(cfg, gallery)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []tagger.TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available for gallery %q", name, gallery)
}
