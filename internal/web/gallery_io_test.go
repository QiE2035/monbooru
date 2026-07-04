package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedImportExportFixture populates the "stock" gallery with a handful of
// rows so round-trip tests have something to compare. Lives on "stock"
// (non-default, non-active) because ImportGallery rejects the active and
// default galleries.
func seedImportExportFixture(t *testing.T, srv *Server) {
	t.Helper()
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
		 VALUES ('seed-sha', 'seed.png', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	// A merged alias pair, so the export includes a canonical_tag_id link.
	alias, err := cx.TagSvc.GetOrCreateTag("cat", 1)
	if err != nil {
		t.Fatalf("create alias tag: %v", err)
	}
	canon, err := cx.TagSvc.GetOrCreateTag("feline", 1)
	if err != nil {
		t.Fatalf("create canon tag: %v", err)
	}
	if err := cx.TagSvc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatalf("query seed image: %v", err)
	}
	if err := cx.TagSvc.AddTagToImage(imgID, canon.ID, false, nil); err != nil {
		t.Fatalf("tag image: %v", err)
	}

	// Drop one physical file into the gallery tree so zip exports have
	// something to package alongside the db.
	if err := os.WriteFile(filepath.Join(cx.GalleryPath, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExportGalleryDB_ProducesValidSQLite(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	// SQLite files start with the 16-byte magic "SQLite format 3\0".
	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("exported DB missing SQLite magic prefix")
	}
	if buf.Len() < 1024 {
		t.Errorf("exported DB suspiciously small: %d bytes", buf.Len())
	}
}

func TestExportGalleryJSON_RoundTripsAliases(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	var exp galleryExport
	if err := json.Unmarshal(buf.Bytes(), &exp); err != nil {
		t.Fatalf("unmarshal: %v\nraw:\n%s", err, buf.String())
	}
	if exp.Version != galleryExportVersion {
		t.Errorf("version = %d, want %d", exp.Version, galleryExportVersion)
	}
	if len(exp.Images) != 1 {
		t.Errorf("images = %d, want 1", len(exp.Images))
	}
	// Alias row must round-trip with its canonical_tag_id populated.
	foundAlias := false
	for _, tag := range exp.Tags {
		if tag.Name == "cat" && tag.IsAlias == 1 && tag.CanonicalTagID.Valid {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Errorf("alias tag not preserved in JSON export, got:\n%s", buf.String())
	}
}

func TestImportGalleryDB_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}

	// Wipe the stock gallery's DB by loading its snapshot back in - the
	// round-trip is the verification, not the state before vs after.
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx := srv.Get("stock")
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("images after import = %d, want 1", n)
	}
	var aliasCount int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 1`).Scan(&aliasCount); err != nil {
		t.Fatal(err)
	}
	if aliasCount != 1 {
		t.Errorf("aliases after import = %d, want 1", aliasCount)
	}
}

func TestImportGalleryJSON_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx := srv.Get("stock")
	var imgCount, aliasCount, imageTagCount int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&imgCount); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 1`).Scan(&aliasCount); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags`).Scan(&imageTagCount); err != nil {
		t.Fatal(err)
	}
	if imgCount != 1 || aliasCount != 1 || imageTagCount != 1 {
		t.Errorf("after JSON import: images=%d aliases=%d image_tags=%d, want 1/1/1",
			imgCount, aliasCount, imageTagCount)
	}
}

func TestImportGalleryJSON_RoundTripsProvenance(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.Get("stock")
	var imgID int64
	if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO image_sources (image_id, site, post_id, url, md5, commentary) VALUES (?, 'danbooru', '42', 'https://x/1', 'abc123', 'artist says hi')`, []any{imgID}},
		{`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, 'pixiv', '', 'https://x/2')`, []any{imgID}},
		{`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, 'danbooru', '42', 1, 2, 3, 4, 'a note box')`, []any{imgID}},
		{`INSERT INTO image_collections (image_id, name, position) VALUES (?, 'extras', 7)`, []any{imgID}},
		{`UPDATE images SET source = 'danbooru', url = 'https://x/1', note = 'operator note' WHERE id = ?`, []any{imgID}},
	} {
		if _, err := cx.DB.Write.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed provenance: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx = srv.Get("stock")
	var site, postID, commentary, md5, note, primary string
	if err := cx.DB.Read.QueryRow(
		`SELECT site, post_id, commentary, md5 FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imgID,
	).Scan(&site, &postID, &commentary, &md5); err != nil {
		t.Fatalf("source row: %v", err)
	}
	if site != "danbooru" || postID != "42" || commentary != "artist says hi" || md5 != "abc123" {
		t.Errorf("primary source = %q/%q/%q/%q, want seeded values", site, postID, commentary, md5)
	}
	var srcCount, annCount, collCount int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_sources WHERE image_id = ?`, imgID).Scan(&srcCount); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_annotations WHERE image_id = ?`, imgID).Scan(&annCount); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_collections WHERE image_id = ? AND name = 'extras'`, imgID).Scan(&collCount); err != nil {
		t.Fatal(err)
	}
	if srcCount != 2 || annCount != 1 || collCount != 1 {
		t.Errorf("after import: sources=%d annotations=%d extra-collections=%d, want 2/1/1", srcCount, annCount, collCount)
	}
	if err := cx.DB.Read.QueryRow(`SELECT note, source FROM images WHERE id = ?`, imgID).Scan(&note, &primary); err != nil {
		t.Fatal(err)
	}
	if note != "operator note" {
		t.Errorf("note = %q, want %q", note, "operator note")
	}
	if primary != "danbooru" {
		t.Errorf("scalar source mirror = %q, want danbooru (oldest row)", primary)
	}
}

func TestImportGalleryJSON_SeedsCollectionsFromSeriesOnOldExports(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	cx := srv.Get("stock")
	if _, err := cx.DB.Write.Exec(`UPDATE images SET series = 'oldbook', series_order = 3 WHERE sha256 = 'seed-sha'`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	var exp galleryExport
	if err := json.Unmarshal(buf.Bytes(), &exp); err != nil {
		t.Fatal(err)
	}
	exp.Version = 2
	exp.ImageCollections = nil
	old, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(old)); err != nil {
		t.Fatalf("import: %v", err)
	}
	cx = srv.Get("stock")
	var name string
	var pos int
	if err := cx.DB.Read.QueryRow(
		`SELECT name, position FROM image_collections WHERE name = 'oldbook'`,
	).Scan(&name, &pos); err != nil {
		t.Fatalf("derived membership: %v", err)
	}
	if pos != 3 {
		t.Errorf("derived position = %d, want 3", pos)
	}
}

func TestImportGalleryJSON_FailedLoadLeavesTargetIntact(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	// A document that decodes fine but fails during load: one image_tags row
	// referencing a tag no tags row defines trips the deferred FK check at
	// commit.
	var exp galleryExport
	if err := json.Unmarshal(buf.Bytes(), &exp); err != nil {
		t.Fatal(err)
	}
	exp.ImageTags = append(exp.ImageTags, imageTagRow{ImageID: exp.Images[0].ID, TagID: 999999})
	bad, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.ImportGallery("stock", "json", bytes.NewReader(bad)); err == nil {
		t.Fatal("import of a bad export succeeded, want error")
	}

	cx := srv.Get("stock")
	var imgCount, tagCount int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&imgCount); err != nil {
		t.Fatal(err)
	}
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags`).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if imgCount != 1 || tagCount != 1 {
		t.Errorf("after failed import: images=%d image_tags=%d, want the original 1/1", imgCount, tagCount)
	}
}

func TestImportGalleryArchive_RestoresImagesAndDB(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryArchive("stock", "db", &buf); err != nil {
		t.Fatal(err)
	}
	// Remove the physical file so we can tell the import put it back.
	cx := srv.Get("stock")
	target := filepath.Join(cx.GalleryPath, "hello.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := srv.ImportGallery("stock", "zip", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("gallery file not restored: %v", err)
	}
}

func TestImportGalleryArchive_ClearsMissingAfterExtract(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}

	// An image whose canonical file lives in the gallery tree, so the archive
	// bundles the bytes and the import must extract them back.
	if err := os.WriteFile(filepath.Join(cx.GalleryPath, "pic.png"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('pic-sha', ?, '', 'png', 5, datetime('now'))`,
		filepath.Join(cx.GalleryPath, "pic.png"),
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryArchive("stock", "db", &buf); err != nil {
		t.Fatal(err)
	}
	// Empty the tree so the pre-extract reconcile can't find the file; only
	// the archive's extracted copy should clear is_missing.
	if err := os.Remove(filepath.Join(cx.GalleryPath, "pic.png")); err != nil {
		t.Fatal(err)
	}

	if err := srv.ImportGallery("stock", "zip", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	cx = srv.Get("stock")
	var missing int
	if err := cx.DB.Read.QueryRow(`SELECT is_missing FROM images WHERE sha256 = 'pic-sha'`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("is_missing = %d after archive import, want 0", missing)
	}
}

func TestImportGallery_RebasesPathsToTargetGallery(t *testing.T) {
	// The export's canonical_path points at the source gallery's root
	// (say /source/gallery). Importing into a differently-mounted target
	// (stock gallery in the multi-gallery fixture) must rewrite every
	// image path onto the target root so links don't dangle.
	srv := newMultiGalleryServer(t)

	// Seed the stock gallery with rows whose canonical_path intentionally
	// lives under a path that is NOT the stock gallery root. Export then
	// import back; rebase must pin everything under stock.
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('sha1', '/foreign/gallery/2024/foo.png', '2024', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical)
		 VALUES ((SELECT id FROM images WHERE sha256 = 'sha1'), '/foreign/gallery/2024/foo.png', 1)`,
	); err != nil {
		t.Fatalf("seed image_path: %v", err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	cx = srv.Get("stock")
	wantPrefix := cx.GalleryPath
	var got string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE sha256 = 'sha1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := wantPrefix + "/2024/foo.png"
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
	var gotAlias string
	if err := cx.DB.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha1')`,
	).Scan(&gotAlias); err != nil {
		t.Fatal(err)
	}
	if gotAlias != want {
		t.Errorf("image_paths.path = %q, want %q", gotAlias, want)
	}
}

// An alias path that lives in a different folder than the canonical
// must keep that folder through the rebase; collapsing it into the
// canonical's folder would lose the operator's hand-maintained
// location and risk a UNIQUE-on-path collision with the canonical
// row. The rebase derives the alias's folder from the inferred
// source root so its relative position survives.
func TestImportGallery_PreservesAliasFolderOnRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)

	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('sha-alias', '/source/gallery/portraits/cat.jpg', 'portraits', 'jpeg', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical)
		 VALUES ((SELECT id FROM images WHERE sha256 = 'sha-alias'),
		         '/source/gallery/portraits/cat.jpg', 1)`,
	); err != nil {
		t.Fatalf("seed canonical alias: %v", err)
	}
	// Alias path lives in a different folder ("photos") under the same
	// source root - an operator who rsync'd the same file twice would
	// register this shape.
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical)
		 VALUES ((SELECT id FROM images WHERE sha256 = 'sha-alias'),
		         '/source/gallery/photos/cat.jpg', 0)`,
	); err != nil {
		t.Fatalf("seed alias in different folder: %v", err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	cx = srv.Get("stock")
	prefix := cx.GalleryPath

	var aliasPath string
	if err := cx.DB.Read.QueryRow(
		`SELECT path FROM image_paths
		 WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha-alias')
		   AND is_canonical = 0`,
	).Scan(&aliasPath); err != nil {
		t.Fatalf("read alias path: %v", err)
	}
	wantAlias := prefix + "/photos/cat.jpg"
	if aliasPath != wantAlias {
		t.Errorf("alias path = %q, want %q (alias folder must survive rebase)", aliasPath, wantAlias)
	}
}

func TestImportGallery_QueuesRebuildThumbsJob(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	// The seeded fixture has one image - the rebuild job kickoff is fire-
	// and-forget, so we accept either "still running" or "already completed"
	// as proof it ran.
	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("no job state after import; expected rebuild-thumbs")
	}
	if state.JobType != "rebuild-thumbs" {
		t.Errorf("job type = %q, want rebuild-thumbs", state.JobType)
	}
}

func TestImportGallery_ActivatesImportedGallery(t *testing.T) {
	// Before the import the active gallery is "default"; after it finishes
	// the imported "stock" gallery becomes active so the rebuild-thumbs
	// job's job-manager lock doesn't leave the user stranded on the
	// previous gallery until the rebuild completes.
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	if srv.activeName != "default" {
		t.Fatalf("pre-import activeName = %q, want default", srv.activeName)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	if srv.activeName != "stock" {
		t.Errorf("post-import activeName = %q, want stock", srv.activeName)
	}
}

// TestImportGallery_FlagsMissingFilesAfterRebase exercises the post-rebase
// is_missing pass: an exported row whose rebased path won't exist in the
// target's filesystem must come out flagged is_missing=1, mirroring how
// Sync handles a file that vanished off disk. Without this the gallery
// view would show a healthy-looking thumbnail that 404s on click.
func TestImportGallery_FlagsMissingFilesAfterRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}

	// Two rows: one whose basename matches a file we'll drop into the
	// stock gallery, and one whose basename is unique to the source so it
	// won't resolve on the target after rebase.
	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	presentPath := filepath.Join(cx.GalleryPath, "present.png")
	if err := os.WriteFile(presentPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('present', '/foreign/gallery/present.png', '', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('gone', '/foreign/gallery/gone.png', '', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{"present", "gone"} {
		if _, err := cx.DB.Write.Exec(
			`INSERT INTO image_paths (image_id, path, is_canonical)
			 VALUES ((SELECT id FROM images WHERE sha256 = ?),
			         (SELECT canonical_path FROM images WHERE sha256 = ?), 1)`,
			sha, sha,
		); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx2 := srv.Get("stock")
	rows := map[string]int{}
	r, err := cx2.DB.Read.Query(`SELECT sha256, is_missing FROM images`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		var sha string
		var miss int
		if err := r.Scan(&sha, &miss); err != nil {
			t.Fatal(err)
		}
		rows[sha] = miss
	}
	if rows["present"] != 0 {
		t.Errorf("present row is_missing = %d, want 0", rows["present"])
	}
	if rows["gone"] != 1 {
		t.Errorf("gone row is_missing = %d, want 1", rows["gone"])
	}
}

func TestImportGallery_RejectsActive(t *testing.T) {
	srv := newMultiGalleryServer(t)
	var buf bytes.Buffer
	// Any bytes - we expect the ctxMu check to fire before we look at content.
	err := srv.ImportGallery("default", "db", &buf)
	if err == nil || !strings.Contains(err.Error(), "active gallery") {
		t.Errorf("expected active-gallery error, got %v", err)
	}
}

func TestExportHandler_ServesDBDownload(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings/galleries/stock/export?format=db", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), `filename="stock.db"`) {
		t.Errorf("missing download filename: %q", w.Header().Get("Content-Disposition"))
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("body missing SQLite magic prefix")
	}
}

func TestImportHandler_RoundTripsStockGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var dbBuf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &dbBuf); err != nil {
		t.Fatal(err)
	}

	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.csrfToken("anon"),
		"confirm_name": "stock",
	}, "file", "stock.db", dbBuf.Bytes())

	h := srv.Handler()
	req := httptest.NewRequest("POST", "/settings/galleries/stock/import", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body: %s", w.Code, w.Body.String())
	}
	// Success writes a flash-ok into the response body; the dialog's
	// client-side after-request hook closes the modal when it sees the
	// flash-ok class.
	resp := w.Body.String()
	if !strings.Contains(resp, "flash-ok") || !strings.Contains(resp, "imported") {
		t.Errorf("expected flash-ok import message, got %q", resp)
	}
}

func TestImportHandler_RejectsBadConfirmName(t *testing.T) {
	srv := newMultiGalleryServer(t)
	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.csrfToken("anon"),
		"confirm_name": "wrong",
	}, "file", "stock.db", []byte("whatever"))

	h := srv.Handler()
	req := httptest.NewRequest("POST", "/settings/galleries/stock/import", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "confirm") {
		t.Errorf("expected confirm-name error, got %q", w.Body.String())
	}
}

func TestSettingsRendersImportExportColumn(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	checks := []string{
		`<th>Export</th>`,
		`<th>Import</th>`,
		`btn-gallery-export`,
		`btn-gallery-import`,
		`id="gallery-export-dialog"`,
		`id="gallery-import-dialog"`,
	}
	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("settings page missing: %q", c)
		}
	}
}

func TestExportArchive_ContainsGalleryFiles(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryArchive("stock", "db", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	seenDB := false
	seenFile := false
	for _, f := range zr.File {
		switch f.Name {
		case "monbooru.db":
			seenDB = true
		case "gallery/hello.txt":
			seenFile = true
		}
	}
	if !seenDB || !seenFile {
		t.Errorf("archive missing entries: db=%v file=%v (%d entries)", seenDB, seenFile, len(zr.File))
	}
}

// buildMultipart is a small helper that emits a multipart body with the given
// text fields and a single file part. Returns the body reader and the
// Content-Type header to set on the request.
func buildMultipart(t *testing.T, fields map[string]string, fileField, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// TestSafeArchiveDest: the central path-traversal defense for every
// zip-import path (gallery import db / json / zip / light, Blombooru
// / Hydrus translators) must reject absolute entries, plain `..`
// traversal, nested `..` after a normal segment, and the equal-prefix
// sibling-directory attack the docstring calls out. Names that
// merely contain `..` as a substring stay legal.
func TestSafeArchiveDest(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		rel     string
		wantErr bool
	}{
		// Legal shapes.
		{"a.png", false},
		{"sub/a.png", false},
		{"deeper/nested/photo.webp", false},
		{"foo..bar.png", false},
		{"a/b/c.png", false},
		// Illegal shapes.
		{"/etc/passwd", true},
		{"../escape.txt", true},
		{"a/../../escape.png", true},
		{"a/../b/../../escape.png", true},
	}
	for _, tc := range cases {
		dst, err := safeArchiveDest(root, tc.rel)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("safeArchiveDest(root, %q) err=%v want err=%v (dst=%q)",
				tc.rel, err, tc.wantErr, dst)
			continue
		}
		if !tc.wantErr {
			rootAbs, _ := filepath.Abs(root)
			if !strings.HasPrefix(dst, rootAbs) {
				t.Errorf("safeArchiveDest(root, %q) returned %q, expected under %q",
					tc.rel, dst, rootAbs)
			}
		}
	}
}
