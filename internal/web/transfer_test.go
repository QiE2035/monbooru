package web

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

func postForm(target string, extra url.Values) *http.Request {
	form := url.Values{"target": {target}}
	for k, v := range extra {
		form[k] = v
	}
	req := httptest.NewRequest("POST", "/images/1/transfer", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestTransferTarget_Rejections(t *testing.T) {
	srv := newMultiGalleryServer(t)
	srv.Get("stock").Degraded = true // distinct from an unknown target
	cases := []struct{ name, target, wantMsg string }{
		{"empty", "", "Pick a target gallery."},
		{"same_gallery", "default", "must be a different gallery"},
		{"unknown", "ghost", "Unknown target gallery."},
		{"degraded", "stock", "target gallery is unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if _, _, ok := srv.transferTarget(w, postForm(tc.target, nil)); ok {
				t.Fatalf("target %q should be rejected", tc.target)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Errorf("body = %q, want to contain %q", w.Body.String(), tc.wantMsg)
			}
		})
	}
}

func TestTransferTarget_AcceptsLiveOtherGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	w := httptest.NewRecorder()
	dst, removeAfter, ok := srv.transferTarget(w, postForm("stock", url.Values{"remove_after": {"1"}}))
	if !ok {
		t.Fatalf("a live other gallery should be accepted: %s", w.Body.String())
	}
	if dst == nil || dst.Name != "stock" {
		t.Errorf("dst = %v, want stock", dst)
	}
	if !removeAfter {
		t.Error("remove_after=1 should parse as a move")
	}
}

func TestRunBatchTransfer_IsolatesFailures(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")
	good := seedRichImage(t, src)

	if err := srv.jobs.Start(models.JobTypeTransfer); err != nil {
		t.Fatalf("start job: %v", err)
	}
	// The missing id fails in transferOneImage; the good one must still land.
	srv.runBatchTransfer([]int64{good.ID, 999999}, dst, false)

	var n int
	if err := dst.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = ?`, good.SHA256).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("good image landed %d times, want 1", n)
	}
	if s := srv.jobs.Get().Summary; !strings.Contains(s, "Transferred 1") || !strings.Contains(s, "1 failed") {
		t.Errorf("summary = %q, want one transferred and one failed", s)
	}
}

func TestRunBatchTransfer_Cancellation(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")
	good := seedRichImage(t, src)

	if err := srv.jobs.Start(models.JobTypeTransfer); err != nil {
		t.Fatalf("start job: %v", err)
	}
	srv.jobs.Cancel() // ctx is already done when the loop starts
	srv.runBatchTransfer([]int64{good.ID}, dst, false)

	if s := srv.jobs.Get().Summary; !strings.Contains(s, "cancelled") {
		t.Errorf("summary = %q, want a cancelled report", s)
	}
	var n int
	if err := dst.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = ?`, good.SHA256).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("cancelled batch transferred %d, want 0", n)
	}
}

// seedRichImage ingests a png into cx and attaches a custom-category tag, a
// rating tag, a source with commentary, an annotation, a note and the favorite
// flag so a transfer has every provenance surface to carry.
func seedRichImage(t *testing.T, cx *galleryCtx) *models.Image {
	t.Helper()
	p := filepath.Join(cx.GalleryPath, "sub", "seed.png")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", "upload")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	cat, err := cx.TagSvc.CreateCategory("mycat", "#123456")
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	tg, err := cx.TagSvc.GetOrCreateTag("special", cat.ID)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{tg.ID, ratingTagIDWeb(t, cx.DB, "explicit")}, false, ""); err != nil {
		t.Fatalf("add tags: %v", err)
	}
	srcTag, err := cx.TagSvc.GetOrCreateTag("fromdanbooru", lookupCategoryID(cx.DB, "general"))
	if err != nil {
		t.Fatalf("create source tag: %v", err)
	}
	if err := cx.TagSvc.AddTagsToImageFromTagger(img.ID, []int64{srcTag.ID}, false, "danbooru"); err != nil {
		t.Fatalf("add source tag: %v", err)
	}
	if err := gallery.AddSourceMembership(cx.DB, img.ID, "danbooru", "123", "https://example.test/posts/123"); err != nil {
		t.Fatalf("add source: %v", err)
	}
	if err := gallery.SetSourceCommentary(cx.DB, img.ID, "danbooru", "123", "artist words"); err != nil {
		t.Fatalf("set commentary: %v", err)
	}
	if err := gallery.ReplaceSourceAnnotations(cx.DB, img.ID, "danbooru", "123",
		[]models.Annotation{{Site: "danbooru", PostID: "123", X: 1, Y: 2, W: 3, H: 4, Body: "a box"}}); err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if err := gallery.AddManualAnnotation(cx.DB, img.ID, 5, 6, 7, 8, "my box"); err != nil {
		t.Fatalf("manual annotation: %v", err)
	}
	if _, err := cx.DB.Write.Exec(`UPDATE images SET note = 'mynote', is_favorited = 1, is_inbox = 0 WHERE id = ?`, img.ID); err != nil {
		t.Fatalf("column setup: %v", err)
	}
	return img
}

func TestTransferOneImage_CopiesFullRecord(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")
	img := seedRichImage(t, src)

	if err := srv.transferOneImage(src, dst, img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}

	if lookupCategoryID(dst.DB, "mycat") == 0 {
		t.Error("custom category was not auto-created in the target")
	}

	// Manual tags stay manual (special + explicit, rating included).
	var manualCount int
	if err := dst.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND is_auto = 0 AND tagger_name IS NULL`, newID).Scan(&manualCount); err != nil {
		t.Fatal(err)
	}
	if manualCount != 2 {
		t.Errorf("manual tags on target = %d, want 2 (special + explicit)", manualCount)
	}
	// A source-attributed tag keeps its origin instead of becoming a user tag.
	var danTagger string
	if err := dst.DB.Read.QueryRow(
		`SELECT it.tagger_name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'fromdanbooru'`, newID).Scan(&danTagger); err != nil {
		t.Fatal(err)
	}
	if danTagger != "danbooru" {
		t.Errorf("transferred source tag tagger_name = %q, want danbooru", danTagger)
	}
	var hasRating bool
	if err := dst.DB.Read.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND tc.name = 'rating' AND t.name = 'explicit')`, newID).Scan(&hasRating); err != nil {
		t.Fatal(err)
	}
	if !hasRating {
		t.Error("rating tag did not ride along")
	}

	var inbox, fav int
	var note string
	if err := dst.DB.Read.QueryRow(`SELECT is_inbox, is_favorited, note FROM images WHERE id = ?`, newID).Scan(&inbox, &fav, &note); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 {
		t.Errorf("is_inbox = %d, want 1 (lands in inbox for review)", inbox)
	}
	if fav != 1 {
		t.Errorf("is_favorited = %d, want 1 (copied)", fav)
	}
	if note != "mynote" {
		t.Errorf("note = %q, want mynote (copied)", note)
	}

	sources, _ := gallery.SourcesForImage(dst.DB, newID)
	if len(sources) != 1 || sources[0].Site != "danbooru" || sources[0].Commentary != "artist words" {
		t.Errorf("sources = %+v, want one danbooru source with commentary", sources)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, newID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("transfer should carry one source box and one manual box, got source=%d manual=%d (%+v)", srcBox, manBox, anns)
	}

	// remove_after was false: source survives.
	var srcExists bool
	if err := src.DB.Read.QueryRow(`SELECT EXISTS(SELECT 1 FROM images WHERE id = ?)`, img.ID).Scan(&srcExists); err != nil {
		t.Fatal(err)
	}
	if !srcExists {
		t.Error("source image should still exist when remove_after is off")
	}
}

func TestTransferOneImage_CarriesAutoTagConfidence(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")

	p := filepath.Join(src.GalleryPath, "conf.png")
	if err := os.WriteFile(p, makePNGBytes(t, 8, 8, 9, 3, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := gallery.Ingest(src.DB, src.GalleryPath, src.ThumbnailsPath, p, "png", "upload")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	tg, err := src.TagSvc.GetOrCreateTag("1girl", lookupCategoryID(src.DB, "general"))
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	conf := 0.875
	if _, err := src.TagSvc.AddTagToImageReportingDup(img.ID, tg.ID, true, &conf, "wd-swinv2"); err != nil {
		t.Fatalf("add auto tag: %v", err)
	}

	if err := srv.transferOneImage(src, dst, img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	var gotConf sql.NullFloat64
	var tagger sql.NullString
	if err := dst.DB.Read.QueryRow(
		`SELECT it.confidence, it.tagger_name FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = '1girl'`, newID).Scan(&gotConf, &tagger); err != nil {
		t.Fatalf("target auto tag: %v", err)
	}
	if !gotConf.Valid || gotConf.Float64 != conf {
		t.Errorf("transferred auto-tag confidence = %v, want %v", gotConf, conf)
	}
	if tagger.String != "wd-swinv2" {
		t.Errorf("transferred auto-tag tagger_name = %q, want wd-swinv2", tagger.String)
	}
}

func TestTransferOneImage_RemoveAfterAndShaDedup(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")

	// Pre-existing target row with the same sha; transfer must merge tags into
	// it rather than insert a duplicate.
	if err := os.WriteFile(filepath.Join(dst.GalleryPath, "dup.png"), makePNGBytes(t, 8, 8, 5, 6, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	preImg, _, err := gallery.Ingest(dst.DB, dst.GalleryPath, dst.ThumbnailsPath, filepath.Join(dst.GalleryPath, "dup.png"), "png", "ingest")
	if err != nil {
		t.Fatal(err)
	}

	img := seedRichImage(t, src) // same pixels -> same sha as preImg
	if img.SHA256 != preImg.SHA256 {
		t.Fatalf("fixtures should share a sha: %s vs %s", img.SHA256, preImg.SHA256)
	}

	if err := srv.transferOneImage(src, dst, img.ID, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var rowCount int
	if err := dst.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = ?`, img.SHA256).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Errorf("target rows for sha = %d, want 1 (deduped)", rowCount)
	}
	var hasSpecial bool
	if err := dst.DB.Read.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'special')`, preImg.ID).Scan(&hasSpecial); err != nil {
		t.Fatal(err)
	}
	if !hasSpecial {
		t.Error("tags were not merged into the existing target row")
	}

	// Provenance merges onto the pre-existing row too: sources, annotations,
	// and a note / favorite it didn't already carry.
	sources, _ := gallery.SourcesForImage(dst.DB, preImg.ID)
	if len(sources) != 1 || sources[0].Site != "danbooru" || sources[0].Commentary != "artist words" {
		t.Errorf("sources = %+v, want one danbooru source with commentary", sources)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, preImg.ID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("dedup transfer should carry one source box and one manual box, got source=%d manual=%d", srcBox, manBox)
	}
	var fav int
	var note string
	if err := dst.DB.Read.QueryRow(`SELECT is_favorited, note FROM images WHERE id = ?`, preImg.ID).Scan(&fav, &note); err != nil {
		t.Fatal(err)
	}
	if fav != 1 {
		t.Errorf("is_favorited = %d, want 1 (raised on merge)", fav)
	}
	if note != "mynote" {
		t.Errorf("note = %q, want mynote (filled on merge)", note)
	}

	// remove_after was true: source is gone.
	var srcExists bool
	if err := src.DB.Read.QueryRow(`SELECT EXISTS(SELECT 1 FROM images WHERE id = ?)`, img.ID).Scan(&srcExists); err != nil {
		t.Fatal(err)
	}
	if srcExists {
		t.Error("source image should be deleted when remove_after is on")
	}
}

func TestTransferOneImage_MergeDoesNotDuplicateManualAnnotations(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")
	img := seedRichImage(t, src)

	// First transfer creates the target row; the second hits the merge path.
	// Operator-drawn boxes must not stack across the re-transfer.
	if err := srv.transferOneImage(src, dst, img.ID, false); err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	if err := srv.transferOneImage(src, dst, img.ID, false); err != nil {
		t.Fatalf("second transfer: %v", err)
	}

	var newID int64
	if err := dst.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, img.SHA256).Scan(&newID); err != nil {
		t.Fatalf("image missing from target: %v", err)
	}
	anns, _ := gallery.AnnotationsForImage(dst.DB, newID)
	srcBox, manBox := 0, 0
	for _, a := range anns {
		if a.Manual {
			manBox++
		} else {
			srcBox++
		}
	}
	if srcBox != 1 || manBox != 1 {
		t.Errorf("re-transfer should stay idempotent, got source=%d manual=%d (%+v)", srcBox, manBox, anns)
	}
}

func TestTransferOneImage_RestoresMissingTarget(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")

	// Pre-existing target row with the same sha, but its file is gone and the
	// row is flagged missing (the state the watcher leaves behind).
	dupPath := filepath.Join(dst.GalleryPath, "dup.png")
	if err := os.WriteFile(dupPath, makePNGBytes(t, 8, 8, 5, 6, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	preImg, _, err := gallery.Ingest(dst.DB, dst.GalleryPath, dst.ThumbnailsPath, dupPath, "png", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dupPath); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.DB.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, preImg.ID); err != nil {
		t.Fatal(err)
	}

	img := seedRichImage(t, src) // same pixels -> same sha as preImg
	if img.SHA256 != preImg.SHA256 {
		t.Fatalf("fixtures should share a sha: %s vs %s", img.SHA256, preImg.SHA256)
	}

	if err := srv.transferOneImage(src, dst, img.ID, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var missing int
	var canon string
	if err := dst.DB.Read.QueryRow(`SELECT is_missing, canonical_path FROM images WHERE id = ?`, preImg.ID).Scan(&missing, &canon); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("is_missing = %d, want 0 (restored on transfer)", missing)
	}
	if _, err := os.Stat(canon); err != nil {
		t.Errorf("target file not restored at %s: %v", canon, err)
	}
}

func TestTransferOneImage_MoveLandsInInbox(t *testing.T) {
	srv := newMultiGalleryServer(t)
	src, dst := srv.Get("default"), srv.Get("stock")
	img := seedRichImage(t, src) // archived at the source (is_inbox = 0)

	if err := srv.transferOneImage(src, dst, img.ID, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var inbox int
	if err := dst.DB.Read.QueryRow(`SELECT is_inbox FROM images WHERE sha256 = ?`, img.SHA256).Scan(&inbox); err != nil {
		t.Fatalf("target row: %v", err)
	}
	if inbox != 1 {
		t.Errorf("is_inbox = %d, want 1 (a move lands in the target inbox for re-filing)", inbox)
	}

	var srcExists bool
	if err := src.DB.Read.QueryRow(`SELECT EXISTS(SELECT 1 FROM images WHERE id = ?)`, img.ID).Scan(&srcExists); err != nil {
		t.Fatal(err)
	}
	if srcExists {
		t.Error("source image should be deleted when remove_after is on")
	}
}
