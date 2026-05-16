package search

import (
	"archive/zip"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
)

func TestParse_BasicTag(t *testing.T) {
	e, err := Parse("cute")
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := e.(TagExpr)
	if !ok {
		t.Fatalf("expected TagExpr, got %T", e)
	}
	if tag.Tag != "cute" {
		t.Errorf("tag = %q", tag.Tag)
	}
}

func TestParse_ImplicitAND(t *testing.T) {
	e, _ := Parse("a b")
	and, ok := e.(AndExpr)
	if !ok {
		t.Fatalf("expected AndExpr, got %T", e)
	}
	left, ok := and.Left.(TagExpr)
	if !ok || left.Tag != "a" {
		t.Errorf("Left = %+v, want TagExpr{a}", and.Left)
	}
	right, ok := and.Right.(TagExpr)
	if !ok || right.Tag != "b" {
		t.Errorf("Right = %+v, want TagExpr{b}", and.Right)
	}
}

func TestParse_OR(t *testing.T) {
	e, _ := Parse("cat OR dog")
	or, ok := e.(OrExpr)
	if !ok {
		t.Fatalf("expected OrExpr, got %T", e)
	}
	_ = or
}

func TestParse_ORChain(t *testing.T) {
	// `a OR b OR c` must produce three leaves, not two; chained ORs past
	// the first pair must not be silently dropped.
	e, _ := Parse("a OR b OR c")
	or, ok := e.(OrExpr)
	if !ok {
		t.Fatalf("expected OrExpr at root, got %T", e)
	}
	leftOr, ok := or.Left.(OrExpr)
	if !ok {
		t.Fatalf("expected nested OrExpr on left, got %T", or.Left)
	}
	if got := leftOr.Left.(TagExpr).Tag; got != "a" {
		t.Errorf("leftOr.Left = %q, want a", got)
	}
	if got := leftOr.Right.(TagExpr).Tag; got != "b" {
		t.Errorf("leftOr.Right = %q, want b", got)
	}
	if got := or.Right.(TagExpr).Tag; got != "c" {
		t.Errorf("or.Right = %q, want c", got)
	}
}

// A bare leading `OR` has no left operand; the parser drops it and
// keeps parsing so the right-hand term stands on its own instead of
// collapsing to an empty expression (which the executor would treat
// as match-all).
func TestParse_LeadingOR_DropsOperator(t *testing.T) {
	e, _ := Parse("OR 1girl")
	tag, ok := e.(TagExpr)
	if !ok {
		t.Fatalf("expected TagExpr after a bare leading OR, got %T (%+v)", e, e)
	}
	if tag.Tag != "1girl" {
		t.Errorf("tag = %q, want 1girl", tag.Tag)
	}
}

// A run of leading ORs collapses the same way - none of them carries
// a left operand, none survives.
func TestParse_LeadingORChain_DropsOperators(t *testing.T) {
	e, _ := Parse("OR OR 1girl")
	tag, ok := e.(TagExpr)
	if !ok {
		t.Fatalf("expected TagExpr, got %T (%+v)", e, e)
	}
	if tag.Tag != "1girl" {
		t.Errorf("tag = %q, want 1girl", tag.Tag)
	}
}

func TestParse_NOT(t *testing.T) {
	e, _ := Parse("-blonde_hair")
	not, ok := e.(NotExpr)
	if !ok {
		t.Fatalf("expected NotExpr, got %T", e)
	}
	_ = not
}

func TestParse_NOT_Keyword(t *testing.T) {
	e, _ := Parse("NOT blonde_hair")
	not, ok := e.(NotExpr)
	if !ok {
		t.Fatalf("expected NotExpr, got %T", e)
	}
	_ = not
}

func TestParse_Filter_Fav(t *testing.T) {
	e, _ := Parse("fav:true")
	f, ok := e.(FilterExpr)
	if !ok {
		t.Fatalf("expected FilterExpr, got %T", e)
	}
	if f.Key != "fav" || f.Val != "true" {
		t.Errorf("filter = {%q, %q}", f.Key, f.Val)
	}
}

func TestParse_Filter_Inbox(t *testing.T) {
	for _, val := range []string{"true", "false"} {
		e, _ := Parse("inbox:" + val)
		f, ok := e.(FilterExpr)
		if !ok {
			t.Fatalf("inbox:%s parsed to %T, want FilterExpr", val, e)
		}
		if f.Key != "inbox" || f.Val != val {
			t.Errorf("inbox:%s parsed as {%q, %q}", val, f.Key, f.Val)
		}
	}
}

func TestParse_Filter_Folder(t *testing.T) {
	e, _ := Parse("folder:2024/jan")
	f, ok := e.(FilterExpr)
	if !ok {
		t.Fatalf("expected FilterExpr, got %T", e)
	}
	if f.Key != "folder" || f.Val != "2024/jan" {
		t.Errorf("filter = {%q, %q}", f.Key, f.Val)
	}
}

func TestParse_Filter_AI(t *testing.T) {
	e, _ := Parse("ai:sd")
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "ai" || f.Val != "sd" {
		t.Errorf("parse ai:sd failed")
	}
}

func TestParse_Filter_Source(t *testing.T) {
	e, _ := Parse("source:my_label")
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "source" || f.Val != "my_label" {
		t.Errorf("parse source:my_label failed")
	}
}

func TestParse_Filter_Type(t *testing.T) {
	for _, val := range []string{"image", "archive", "image,archive"} {
		e, _ := Parse("type:" + val)
		f, ok := e.(FilterExpr)
		if !ok || f.Key != "type" || f.Val != val {
			t.Errorf("type:%s parsed as %T %+v", val, e, e)
		}
	}
}

func TestParse_Filter_Collection(t *testing.T) {
	e, _ := Parse("collection:naruto")
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "collection" || f.Val != "naruto" {
		t.Errorf("parse collection:naruto failed: %+v", e)
	}
}

func TestParse_Filter_Pages(t *testing.T) {
	for _, val := range []string{">=100", "<200", "=42", "0"} {
		e, _ := Parse("pages:" + val)
		f, ok := e.(FilterExpr)
		if !ok || f.Key != "pages" || f.Val != val {
			t.Errorf("pages:%s parsed as %T %+v", val, e, e)
		}
	}
}

func TestParse_Wildcard_Prefix(t *testing.T) {
	e, _ := Parse("blue*")
	tag, ok := e.(TagExpr)
	if !ok || tag.Wildcard != "prefix" || tag.Tag != "blue" {
		t.Errorf("expected prefix wildcard, got %+v", e)
	}
}

func TestParse_Wildcard_Substring(t *testing.T) {
	e, _ := Parse("*blue*")
	tag, ok := e.(TagExpr)
	if !ok || tag.Wildcard != "substring" || tag.Tag != "blue" {
		t.Errorf("expected substring wildcard, got %+v", e)
	}
}

func TestParse_Wildcard_Suffix(t *testing.T) {
	// `*xyz` (leading star, no trailing star) is the suffix wildcard.
	// Substring requires both anchors and prefix requires a trailing
	// star, so the suffix-only form lives in its own branch.
	e, _ := Parse("*eyes")
	tag, ok := e.(TagExpr)
	if !ok || tag.Wildcard != "suffix" || tag.Tag != "eyes" {
		t.Errorf("expected suffix wildcard, got %+v", e)
	}
}

func TestParse_Empty(t *testing.T) {
	e, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Error("expected nil for empty query")
	}
}

// A bare `*` would otherwise become TagExpr{Tag:"", Wildcard:"prefix"}
// and the executor would emit `LIKE '%' ESCAPE '\'`, matching every
// tag - a "select all" alias the documented syntax doesn't expose.
// The parser collapses it to a literal-no-match so it composes
// predictably with the rest of the query.
func TestParse_BareWildcard_NoMatch(t *testing.T) {
	for _, in := range []string{"*", "**"} {
		e, err := Parse(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		tag, ok := e.(TagExpr)
		if !ok {
			t.Fatalf("expected TagExpr for %q, got %T", in, e)
		}
		if tag.Tag != "" || tag.Wildcard == "prefix" {
			t.Errorf("Parse(%q) = %+v, want empty literal-no-match", in, tag)
		}
	}
}

func TestBuildWhere_FolderFilter(t *testing.T) {
	// folder:PATH is recursive: images in PATH or any subfolder under it.
	expr := FilterExpr{Key: "folder", Val: "2024/jan"}
	where, args, _ := buildWhere(expr)
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	if args[0] != "2024/jan" {
		t.Errorf("arg[0] = %v", args[0])
	}
	if args[1] != "2024/jan/%" {
		t.Errorf("arg[1] = %v", args[1])
	}
	if !strings.Contains(where, "folder_path") || !strings.Contains(where, "LIKE") {
		t.Errorf("where clause should combine exact and LIKE match: %s", where)
	}
}

func TestBuildWhere_FolderFilterEmpty(t *testing.T) {
	// `folder:` (empty value) is now the recursive-root case: every
	// non-missing image lives at or under the root, so the filter is a
	// no-op (`1=1`). Root-only stays reachable via `folderonly:`.
	expr := FilterExpr{Key: "folder", Val: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args for empty folder, got %d", len(args))
	}
	if strings.Contains(where, "folder_path") {
		t.Errorf("empty folder: should no longer constrain folder_path: %s", where)
	}
}

func TestBuildWhere_FolderOnlyFilter(t *testing.T) {
	// folderonly:PATH is exact: only images whose folder_path matches verbatim.
	expr := FilterExpr{Key: "folderonly", Val: "2024/jan"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(args))
	}
	if args[0] != "2024/jan" {
		t.Errorf("arg[0] = %v", args[0])
	}
	if !strings.Contains(where, "folder_path = ?") {
		t.Errorf("where clause should match folder_path exactly: %s", where)
	}
}

func TestBuildWhere_FolderOnlyFilterEmpty(t *testing.T) {
	// folderonly: (empty) is the gallery root directly, no recursion.
	expr := FilterExpr{Key: "folderonly", Val: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args for empty folderonly, got %d", len(args))
	}
	if !strings.Contains(where, "folder_path = ''") {
		t.Errorf("where clause for root-only should match empty folder_path: %s", where)
	}
}

func TestBuildWhere_MissingTrue(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "true"}
	_, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true")
	}
}

func TestBuildWhere_TypeAnimated(t *testing.T) {
	expr := FilterExpr{Key: "type", Val: "animated"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "file_type IN ('gif', 'mp4', 'webm')") {
		t.Errorf("where clause missing animated set: %s", where)
	}
}

func TestBuildWhere_TypeAnimatedNegated(t *testing.T) {
	expr := NotExpr{Expr: FilterExpr{Key: "type", Val: "animated"}}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT") || !strings.Contains(where, "file_type IN ('gif', 'mp4', 'webm')") {
		t.Errorf("where clause missing negated animated set: %s", where)
	}
}

func TestBuildWhere_TaggedTrue(t *testing.T) {
	expr := FilterExpr{Key: "tagged", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing tagged subselect: %s", where)
	}
	if strings.Contains(where, "NOT EXISTS") {
		t.Errorf("tagged:true should match tagged images, got: %s", where)
	}
}

func TestBuildWhere_TaggedFalse(t *testing.T) {
	expr := FilterExpr{Key: "tagged", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing untagged subselect: %s", where)
	}
}

func TestBuildWhere_AutotaggedTrue(t *testing.T) {
	expr := FilterExpr{Key: "autotagged", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "it.is_auto = 1") {
		t.Errorf("where clause missing is_auto filter: %s", where)
	}
	if strings.Contains(where, "NOT EXISTS") {
		t.Errorf("autotagged:true should match auto-tagged images, got: %s", where)
	}
}

func TestBuildWhere_AutotaggedFalse(t *testing.T) {
	expr := FilterExpr{Key: "autotagged", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing NOT EXISTS: %s", where)
	}
	if !strings.Contains(where, "it.is_auto = 1") {
		t.Errorf("where clause missing is_auto filter: %s", where)
	}
}

// Integration test with real DB
type searchEnv struct {
	db            *db.DB
	galleryDir    string
	thumbnailsDir string
	maxFileSizeMB int
}

func setupSearchDB(t *testing.T) (*db.DB, *searchEnv) {
	t.Helper()
	tmpDir := t.TempDir()
	galleryDir := filepath.Join(tmpDir, "gallery")
	os.MkdirAll(galleryDir, 0755)

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return database, &searchEnv{
		db:            database,
		galleryDir:    galleryDir,
		thumbnailsDir: filepath.Join(tmpDir, "thumbs"),
		maxFileSizeMB: 100,
	}
}

var ingestCounter int

func ingestTestImage(t *testing.T, database *db.DB, env *searchEnv, name string) {
	t.Helper()
	ingestCounter++
	img := image.NewRGBA(image.Rect(0, 0, 10+ingestCounter, 10))
	path := filepath.Join(env.galleryDir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png", ""); err != nil {
		t.Fatalf("ingest %q: %v", name, err)
	}
}

func TestExecute_BasicSearch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "test.png")

	q := Query{Sort: "newest", Page: 1, Limit: 40}
	result, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
}

func TestExecute_FolderFilter(t *testing.T) {
	database, env := setupSearchDB(t)

	subDir := filepath.Join(env.galleryDir, "2024")
	os.MkdirAll(subDir, 0755)
	ingestTestImage(t, database, env, "root.png")

	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	path := filepath.Join(subDir, "sub.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png", ""); err != nil {
		t.Fatalf("ingest sub.png: %v", err)
	}

	// Search for gallery root only
	if _, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "folder", Val: ""},
		Page:  1,
		Limit: 40,
	}); err != nil {
		t.Fatal(err)
	}
}

// ingestTestManga writes a tiny one-page cbz into the gallery and
// ingests it. Returns the new image id. Each call produces a unique
// page bitmap so successive calls don't dedup against each other.
func ingestTestManga(t *testing.T, database *db.DB, env *searchEnv, name string, series string) int64 {
	t.Helper()
	ingestCounter++
	pic := image.NewRGBA(image.Rect(0, 0, 8+ingestCounter, 8))
	cbzPath := filepath.Join(env.galleryDir, name)
	f, err := os.Create(cbzPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("01.png")
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := png.Encode(w, pic); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	rec, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, cbzPath, "cbz", "")
	if err != nil {
		t.Fatalf("Ingest cbz: %v", err)
	}
	if series != "" {
		if _, err := database.Write.Exec(`UPDATE images SET series = ? WHERE id = ?`, series, rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	return rec.ID
}

func TestExecute_TypeArchive_FindsCBZRow(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "regular.png")
	ingestTestManga(t, database, env, "m.cbz", "")

	res, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "type", Val: "archive"},
		Page:  1, Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("type:archive total = %d, want 1", res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].FileType != "cbz" {
		t.Errorf("type:archive returned %+v", res.Results)
	}
}

func TestExecute_TypeImage_ExcludesManga(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "regular.png")
	ingestTestManga(t, database, env, "m.cbz", "")

	res, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "type", Val: "image"},
		Page:  1, Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("type:image total = %d, want 1", res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].FileType == "cbz" {
		t.Errorf("type:image returned %+v", res.Results)
	}
}

func TestExecute_SystemFilter_NoMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	res, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "system", Val: ""},
		Page:  1, Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Errorf("system: total = %d, want 0 (autocomplete-only trigger)", res.Total)
	}
}

func TestExecute_CollectionExactMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestManga(t, database, env, "naruto.cbz", "Naruto")
	ingestTestManga(t, database, env, "bleach.cbz", "Bleach")

	res, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "collection", Val: "Naruto"},
		Page:  1, Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("collection:Naruto total = %d, want 1", res.Total)
	}
}

func TestExecute_PagesComparison(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "regular.png")
	ingestTestManga(t, database, env, "small.cbz", "")
	mid := ingestTestManga(t, database, env, "mid.cbz", "")
	if _, err := database.Write.Exec(`UPDATE images SET page_count = 100 WHERE id = ?`, mid); err != nil {
		t.Fatal(err)
	}

	res, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "pages", Val: ">=10"},
		Page:  1, Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("pages:>=10 total = %d, want 1 (only the 100-page manga)", res.Total)
	}
}

func TestExecute_TagAliasResolvesToCanonical(t *testing.T) {
	// After a merge the image_tags row lives on the canonical; searching
	// for the alias name must still surface the image.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "alias_search.png")

	var imgID, generalID int64
	database.Read.QueryRow(`SELECT id FROM images LIMIT 1`).Scan(&imgID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID, aliasID int64
	database.Write.QueryRow(`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, "feline", generalID).Scan(&canonID)
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES (?, ?, 1, ?) RETURNING id`,
		"cat", generalID, canonID,
	).Scan(&aliasID)
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, canonID,
	); err != nil {
		t.Fatal(err)
	}
	// match the AddTagToImage invariant fastTagTotal trusts.
	if _, err := database.Write.Exec(
		`UPDATE tags SET usage_count = 1 WHERE id = ?`, canonID,
	); err != nil {
		t.Fatal(err)
	}

	result, err := Execute(database, Query{
		Expr:  TagExpr{Tag: "cat"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1 (alias should resolve to canonical)", result.Total)
	}
}

// ratingTagID looks up a seeded rating tag by name; the schema bootstrap
// inserts the four canonical rows so they're always available in tests.
func ratingTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

// getOrCreateTagID returns the id of a general-category tag with the
// given name, creating the row if absent. Used by tests that don't go
// through the tags.Service helper layer.
func getOrCreateTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id
	}
	var catID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&catID); err != nil {
		t.Fatalf("look up general category: %v", err)
	}
	res, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, name, catID,
	)
	if err != nil {
		t.Fatalf("insert tag %q: %v", name, err)
	}
	id, _ = res.LastInsertId()
	return id
}

// attachTag attaches an image_tags row and bumps usage_count to match.
func attachTag(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imageID, tagID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID,
	); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_RatingHighestWins(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rating_a.png")
	ingestTestImage(t, database, env, "rating_b.png")

	var idA, idB int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%rating_a.png'`).Scan(&idA)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%rating_b.png'`).Scan(&idB)

	// idA carries general only; idB carries general+explicit. Only idB
	// should match rating:explicit; only idA should match rating:general.
	attachTag(t, database, idA, ratingTagID(t, database, "general"))
	attachTag(t, database, idB, ratingTagID(t, database, "general"))
	attachTag(t, database, idB, ratingTagID(t, database, "explicit"))

	cases := []struct {
		val   string
		want  int64
		count int
	}{
		{"general", idA, 1},
		{"explicit", idB, 1},
		{"sensitive", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			result, err := Execute(database, Query{
				Expr:  FilterExpr{Key: "rating", Val: tc.val},
				Page:  1,
				Limit: 40,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Total != tc.count {
				t.Fatalf("Total = %d, want %d", result.Total, tc.count)
			}
			if tc.count == 1 && result.Results[0].ID != tc.want {
				t.Errorf("matched id = %d, want %d", result.Results[0].ID, tc.want)
			}
		})
	}
}

func TestExecute_RatingCeilingHidesHigher(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ceil_safe.png")
	ingestTestImage(t, database, env, "ceil_explicit.png")
	ingestTestImage(t, database, env, "ceil_untagged.png")

	var safeID, expID, untagID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_safe.png'`).Scan(&safeID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_explicit.png'`).Scan(&expID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_untagged.png'`).Scan(&untagID)

	attachTag(t, database, safeID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	// Ceiling = sensitive: NOT rating:questionable AND NOT rating:explicit.
	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2 (general + untagged pass; explicit hidden)", result.Total)
	}
	for _, img := range result.Results {
		if img.ID == expID {
			t.Errorf("explicit image %d leaked through the ceiling", expID)
		}
	}
}

func TestFastCountCeiling_MatchesSlowPath(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fc_general.png")
	ingestTestImage(t, database, env, "fc_explicit.png")
	ingestTestImage(t, database, env, "fc_untagged.png")

	var genID, expID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fc_general.png'`).Scan(&genID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fc_explicit.png'`).Scan(&expID)
	attachTag(t, database, genID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	got, ok := fastCountCeiling(database, expr)
	if !ok {
		t.Fatal("fastCountCeiling should recognise the ceiling AST")
	}
	if got != 2 {
		t.Errorf("fastCountCeiling = %d, want 2", got)
	}
}

// TestFastCountCeiling_WrappedUserExpr exercises the cookie-applied
// shape: AndExpr{userExpr, ceilingChain}. The helper defers to the
// slow exact COUNT in this case because min(userCount, chainBound) is
// only a loose upper bound on the actual intersection - the fixture
// here has blue_eyes=2 and chainBound=3 but only 1 image satisfies
// both, and the slow COUNT must surface that exact total to keep
// pagination from advertising phantom trailing pages.
func TestFastCountCeiling_WrappedUserExpr(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "wc_safe_blue.png")
	ingestTestImage(t, database, env, "wc_safe_red.png")
	ingestTestImage(t, database, env, "wc_explicit_blue.png")
	ingestTestImage(t, database, env, "wc_untagged.png")

	var safeBlueID, safeRedID, expBlueID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wc_safe_blue.png'`).Scan(&safeBlueID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wc_safe_red.png'`).Scan(&safeRedID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wc_explicit_blue.png'`).Scan(&expBlueID)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	var blueID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blue_eyes', ?, 0) RETURNING id`,
		generalID,
	).Scan(&blueID)
	attachTag(t, database, safeBlueID, blueID)
	attachTag(t, database, expBlueID, blueID)
	attachTag(t, database, safeBlueID, ratingTagID(t, database, "general"))
	attachTag(t, database, safeRedID, ratingTagID(t, database, "general"))
	attachTag(t, database, expBlueID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left: TagExpr{Tag: "blue_eyes"},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	if _, ok := fastCountCeiling(database, expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT for wrapped userExpr")
	}

	// Execute end-to-end runs the slow exact COUNT and resolves to the
	// actual single match - no phantom trailing page for blue_eyes #2.
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1 (exact, not the loose min(2,3) upper bound)", result.Total)
	}
	if len(result.Results) != 1 || result.Results[0].ID != safeBlueID {
		t.Errorf("Execute results = %+v, want only safeBlueID=%d", result.Results, safeBlueID)
	}
}

// TestFastCountCeiling_WrappedNoFastBound covers an AndExpr{userExpr,
// chain} where userExpr is a shape fastTagTotal can't bound (a fav:
// filter here). chainBound alone overshoots a narrow userExpr by orders
// of magnitude, so fastCountCeiling bails and lets the slow exact
// COUNT serve the total. Execute end-to-end must still resolve to the
// real match set.
func TestFastCountCeiling_WrappedNoFastBound(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "nfb_a.png")
	ingestTestImage(t, database, env, "nfb_b.png")
	ingestTestImage(t, database, env, "nfb_c.png")

	var aID, bID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%nfb_a.png'`).Scan(&aID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%nfb_b.png'`).Scan(&bID)
	attachTag(t, database, aID, ratingTagID(t, database, "general"))
	attachTag(t, database, bID, ratingTagID(t, database, "explicit"))

	expr := AndExpr{
		Left:  FilterExpr{Key: "fav", Val: "true"},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	if _, ok := fastCountCeiling(database, expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT when userExpr can't be fast-bounded")
	}
	// Execute end-to-end resolves the exact total via the slow COUNT;
	// the test fixture has no favorites so the match set is empty.
	res, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 || len(res.Results) != 0 {
		t.Errorf("Execute total=%d results=%d, want 0/0", res.Total, len(res.Results))
	}
}

// TestExecute_CeilingWithMultiAndUserExpr pins the bug where an AND-N
// user expression under an active rating ceiling reported the SFW
// visible count as the search total, generating phantom empty pages
// and starving the cache populate gate. The slow COUNT path now serves
// the exact total.
func TestExecute_CeilingWithMultiAndUserExpr(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ceil_match_safe.png")
	ingestTestImage(t, database, env, "ceil_match_explicit.png")
	ingestTestImage(t, database, env, "ceil_partial.png")
	ingestTestImage(t, database, env, "ceil_other.png")

	var matchSafeID, matchExpID, partialID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_match_safe.png'`).Scan(&matchSafeID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_match_explicit.png'`).Scan(&matchExpID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ceil_partial.png'`).Scan(&partialID)

	var generalCatID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalCatID)
	tagID := func(name string) int64 {
		var id int64
		if err := database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0) RETURNING id`,
			name, generalCatID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, c, d := tagID("alpha"), tagID("bravo"), tagID("charlie"), tagID("delta")
	for _, id := range []int64{a, b, c, d} {
		attachTag(t, database, matchSafeID, id)
		attachTag(t, database, matchExpID, id)
	}
	// partialID carries only three of the four; must not match.
	for _, id := range []int64{a, b, c} {
		attachTag(t, database, partialID, id)
	}
	attachTag(t, database, matchSafeID, ratingTagID(t, database, "general"))
	attachTag(t, database, matchExpID, ratingTagID(t, database, "explicit"))

	// userExpr: alpha AND bravo AND charlie AND delta; ceiling: SFW.
	user := AndExpr{Left: AndExpr{Left: AndExpr{Left: TagExpr{Tag: "alpha"}, Right: TagExpr{Tag: "bravo"}}, Right: TagExpr{Tag: "charlie"}}, Right: TagExpr{Tag: "delta"}}
	chain := AndExpr{Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}}, Right: AndExpr{Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}}, Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}}}}
	expr := AndExpr{Left: user, Right: chain}

	res, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40, CacheKey: "ceiling-and4"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (only ceil_match_safe carries all four tags AND is sfw)", res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].ID != matchSafeID {
		t.Errorf("Results = %+v, want only matchSafeID=%d", res.Results, matchSafeID)
	}
}

// TestExecute_CeilingCategoryQualifiedExact pins the user-reported
// shape: a category-qualified tag under a SFW ceiling whose carriers
// span multiple rating levels. The fixture has a popular general tag
// on five images (1 sfw + 2 sensitive + 2 explicit) plus one extra
// sfw filler; the loose min(userCount=5, chainBound=2) overshoot used
// to advertise two trailing pages, when only the single sfw carrier
// of the tag actually matches. Exercises both directions of the
// overshoot via two queries against the same fixture.
func TestExecute_CeilingCategoryQualifiedExact(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ccq_pop_safe.png")
	ingestTestImage(t, database, env, "ccq_pop_sens1.png")
	ingestTestImage(t, database, env, "ccq_pop_sens2.png")
	ingestTestImage(t, database, env, "ccq_pop_exp1.png")
	ingestTestImage(t, database, env, "ccq_pop_exp2.png")
	ingestTestImage(t, database, env, "ccq_other_safe.png")

	idOf := func(name string) int64 {
		var id int64
		database.Read.QueryRow(
			`SELECT id FROM images WHERE canonical_path LIKE '%' || ?`, name,
		).Scan(&id)
		return id
	}
	popSafe := idOf("ccq_pop_safe.png")
	popSens1 := idOf("ccq_pop_sens1.png")
	popSens2 := idOf("ccq_pop_sens2.png")
	popExp1 := idOf("ccq_pop_exp1.png")
	popExp2 := idOf("ccq_pop_exp2.png")
	otherSafe := idOf("ccq_other_safe.png")

	var generalCatID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalCatID)
	var popID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('popular', ?, 0) RETURNING id`,
		generalCatID,
	).Scan(&popID)
	for _, id := range []int64{popSafe, popSens1, popSens2, popExp1, popExp2} {
		attachTag(t, database, id, popID)
	}
	attachTag(t, database, popSafe, ratingTagID(t, database, "general"))
	attachTag(t, database, popSens1, ratingTagID(t, database, "sensitive"))
	attachTag(t, database, popSens2, ratingTagID(t, database, "sensitive"))
	attachTag(t, database, popExp1, ratingTagID(t, database, "explicit"))
	attachTag(t, database, popExp2, ratingTagID(t, database, "explicit"))
	attachTag(t, database, otherSafe, ratingTagID(t, database, "general"))

	user := FilterExpr{Key: "general", Val: "popular"}
	chain := AndExpr{
		Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "sensitive"}},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	expr := AndExpr{Left: user, Right: chain}

	if _, ok := fastCountCeiling(database, expr); ok {
		t.Fatal("fastCountCeiling should defer to slow COUNT for wrapped userExpr")
	}
	res, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40, CacheKey: "ccq-popular"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (only popSafe is sfw); loose min(userCount=5, chainBound=2)=2 overshoots",
			res.Total)
	}
	if len(res.Results) != 1 || res.Results[0].ID != popSafe {
		t.Errorf("Results = %+v, want only popSafe=%d", res.Results, popSafe)
	}
}

// TestFastCountGenerated_HashMatch verifies the helper resolves
// generated:HASH via the metadata partial indexes. Both sd_metadata
// and comfyui_metadata carriers count, dedup'd via UNION.
func TestFastCountGenerated_HashMatch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fg_sd.png")
	ingestTestImage(t, database, env, "fg_comfy.png")
	ingestTestImage(t, database, env, "fg_both.png")
	ingestTestImage(t, database, env, "fg_other.png")
	ingestTestImage(t, database, env, "fg_missing.png")

	var sdID, comfyID, bothID, otherID, missingID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fg_sd.png'`).Scan(&sdID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fg_comfy.png'`).Scan(&comfyID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fg_both.png'`).Scan(&bothID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fg_other.png'`).Scan(&otherID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fg_missing.png'`).Scan(&missingID)

	insertSD := func(id int64, hash string) {
		if _, err := database.Write.Exec(
			`INSERT INTO sd_metadata (image_id, generation_hash) VALUES (?, ?)`, id, hash,
		); err != nil {
			t.Fatal(err)
		}
	}
	insertComfy := func(id int64, hash string) {
		if _, err := database.Write.Exec(
			`INSERT INTO comfyui_metadata (image_id, generation_hash) VALUES (?, ?)`, id, hash,
		); err != nil {
			t.Fatal(err)
		}
	}
	insertSD(sdID, "abc123")
	insertComfy(comfyID, "abc123")
	insertSD(bothID, "abc123")
	insertComfy(bothID, "abc123")
	insertSD(otherID, "deadbeef")
	insertSD(missingID, "abc123")
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, missingID); err != nil {
		t.Fatal(err)
	}

	got, ok := fastCountGenerated(database, FilterExpr{Key: "generated", Val: "abc123"})
	if !ok {
		t.Fatal("fastCountGenerated should answer for a known hash")
	}
	if got != 3 {
		t.Errorf("generated:abc123 = %d, want 3 (sd + comfy + both; missing excluded)", got)
	}

	got, ok = fastCountGenerated(database, FilterExpr{Key: "generated", Val: "true"})
	if !ok {
		t.Fatal("fastCountGenerated should answer for any value")
	}
	if got != 0 {
		t.Errorf("generated:true = %d, want 0 (literal hash 'true' matches nothing)", got)
	}
}

// TestFastCountRating verifies that rating:LEVEL short-circuits to the
// rating tag's usage_count when the bound is large enough to matter.
// The highest level (explicit) is always exact - usage_count is the
// answer, no higher level can hide a carrier. Lower levels gate on
// fastApproxThreshold so small/test fixtures stay on the slow path's
// exact count and only large libraries pay the upper-bound trade.
func TestFastCountRating(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fr_general.png")
	ingestTestImage(t, database, env, "fr_explicit.png")
	ingestTestImage(t, database, env, "fr_untagged.png")

	var genID, expID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fr_general.png'`).Scan(&genID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fr_explicit.png'`).Scan(&expID)
	attachTag(t, database, genID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))

	cases := []struct {
		val  string
		want int
		ok   bool
	}{
		// Highest rank: always fast, count is exact.
		{"explicit", 1, true},
		// Lower ranks below fastApproxThreshold fall through; the slow
		// path takes over and produces the highest-wins exact count.
		{"general", 0, false},
		// Empty rating tag (usage_count = 0) is exact at any size.
		{"sensitive", 0, true},
		{"questionable", 0, true},
		// Out-of-vocabulary level matches nothing.
		{"not_a_level", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			got, ok := fastCountRating(database, FilterExpr{Key: "rating", Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFastCountFolder covers the recursive folder count: same match
// set as the slow path's (folder = ? OR folder LIKE ?), but two
// index-pinned seeks against the partial idx_images_folder_visible
// instead of a SCAN. The half-open range trick rests on '0' (0x30)
// being the codepoint immediately after '/' (0x2f).
func TestFastCountFolder(t *testing.T) {
	database, env := setupSearchDB(t)

	for i, name := range []string{"root.png", "anime_a.png", "anime_b.png", "anime_sub.png", "deep_x.png", "deep_y.png", "anime_other.png"} {
		ingestTestImage(t, database, env, name)
		// Pin the folder_path of each image deterministically so the
		// fixture exercises every shape: root, exact dir, nested dir,
		// a sibling that should NOT match.
		var folder string
		switch i {
		case 0:
			folder = ""
		case 1, 2:
			folder = "anime"
		case 3:
			folder = "anime/girls"
		case 4, 5:
			folder = "anime/girls/blue_eyes"
		case 6:
			folder = "anime_other" // sibling sharing the prefix; must NOT match folder:anime
		}
		if _, err := database.Write.Exec(
			`UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%' || ? || '%'`,
			folder, name,
		); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		val  string
		want int
		ok   bool
	}{
		// folder:anime should match the two anime/* images plus the
		// nested anime/girls and anime/girls/blue_eyes images, but
		// NOT the anime_other sibling.
		{"anime", 5, true},
		// folder:anime/girls catches its three (one direct, two nested).
		{"anime/girls", 3, true},
		// Empty folder: helper bails (slow path's "1=1" semantic).
		{"", 0, false},
		// Non-matching prefix returns exact 0.
		{"missing_dir", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			got, ok := fastCountFolder(database, FilterExpr{Key: "folder", Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFastCountFolder_MatchesSlowPath verifies the helper agrees with
// Execute's count phase under the same expression - the slow OR-LIKE
// shape and the helper's index seeks must produce the same total.
func TestFastCountFolder_MatchesSlowPath(t *testing.T) {
	database, env := setupSearchDB(t)

	for _, name := range []string{"a.png", "b.png", "c.png", "d.png"} {
		ingestTestImage(t, database, env, name)
	}
	if _, err := database.Write.Exec(`UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%a.png'`, "movies"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%b.png'`, "movies/scifi"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET folder_path = ? WHERE canonical_path LIKE '%c.png'`, "movies_other"); err != nil {
		t.Fatal(err)
	}

	result, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "folder", Val: "movies"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Errorf("Execute Total = %d, want 2 (movies + movies/scifi; movies_other excluded)", result.Total)
	}
}

// TestFastCountAI_Csv covers the comma-separated form: source_type
// values matching the 4-LIKE pattern get summed via index-pinned
// COUNT(*) WHERE source_type IN (...). Same match set as the slow
// path; only the count query restructures.
func TestFastCountAI_Csv(t *testing.T) {
	database, env := setupSearchDB(t)

	// Seed images with each source_type value the metadata extractor
	// produces. The bulk-insert mirrors what ingestion would land.
	for _, st := range []struct {
		name       string
		sourceType string
	}{
		{"src_a.png", "a1111"},
		{"src_b.png", "comfyui"},
		{"src_ab.png", "a1111,comfyui"},
		{"src_none.png", "none"},
	} {
		ingestTestImage(t, database, env, st.name)
		if _, err := database.Write.Exec(
			`UPDATE images SET source_type = ? WHERE canonical_path LIKE '%' || ? || '%'`,
			st.sourceType, st.name,
		); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		val  string
		want int
		ok   bool
	}{
		// CSV value matches both the exact "a1111,comfyui" image and
		// nothing else (no values like "X,a1111,comfyui" exist).
		{"a1111,comfyui", 1, true},
		// Bare value: slow path already pins the index, helper bails.
		{"a1111", 0, false},
		// Special aliases stay on the slow path.
		{"any", 0, false},
		{"none", 0, false},
		{"sd", 0, false},
		// CSV value matching nothing returns exact 0.
		{"x,y", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			got, ok := fastCountAI(database, FilterExpr{Key: "ai", Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFastCountTagged_PartitionsVisible pins the fix for U-F012:
// `tagged:true` and `tagged:false` must partition the visible image
// set (sum = visible_total, no overlap). Mixed fixture: one tagged,
// one untagged, one auto-tagged.
func TestFastCountTagged_PartitionsVisible(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ft_tagged.png")
	ingestTestImage(t, database, env, "ft_untagged.png")
	ingestTestImage(t, database, env, "ft_auto.png")
	var taggedID, autoID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ft_tagged.png'`).Scan(&taggedID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ft_auto.png'`).Scan(&autoID)
	tagID := getOrCreateTagID(t, database, "blue")
	attachTag(t, database, taggedID, tagID)
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 1)`, autoID, tagID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		key, val string
		want     int
		ok       bool
	}{
		{"tagged", "true", 2, true},
		{"autotagged", "true", 1, true},
		// :false stays on the slow path; the helper falls through.
		{"tagged", "false", 0, false},
		{"autotagged", "false", 0, false},
		{"rating", "true", 0, false},
	}
	for _, tc := range cases {
		name := tc.key + ":" + tc.val
		t.Run(name, func(t *testing.T) {
			got, ok := fastCountTagged(database, FilterExpr{Key: tc.key, Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}

	// Cross-check Execute round-trips the partition: tagged:true total
	// + tagged:false total = visible total, and the result rows obey
	// the same predicate.
	tres, err := Execute(database, Query{Expr: FilterExpr{Key: "tagged", Val: "true"}, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	fres, err := Execute(database, Query{Expr: FilterExpr{Key: "tagged", Val: "false"}, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if tres.Total+fres.Total != 3 {
		t.Errorf("tagged:true+tagged:false = %d, want 3 (no overlap)", tres.Total+fres.Total)
	}
	for _, img := range tres.Results {
		if img.ID != taggedID && img.ID != autoID {
			t.Errorf("tagged:true returned untagged image id=%d", img.ID)
		}
	}
}

// TestFastCountRating_RoutesThroughExecute pins that the helper is
// wired into Execute's count phase: an Execute against rating:explicit
// returns the same total the helper would.
func TestFastCountRating_RoutesThroughExecute(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fre_a.png")
	ingestTestImage(t, database, env, "fre_b.png")
	var aID, bID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fre_a.png'`).Scan(&aID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%fre_b.png'`).Scan(&bID)
	attachTag(t, database, aID, ratingTagID(t, database, "explicit"))
	attachTag(t, database, bID, ratingTagID(t, database, "explicit"))

	result, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "rating", Val: "explicit"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("len(Results) = %d, want 2", len(result.Results))
	}
}

func TestExtractCeilingShape(t *testing.T) {
	tests := []struct {
		name       string
		expr       Expr
		wantUser   Expr
		wantLevels []string
		wantOK     bool
	}{
		{
			name:       "nil",
			expr:       nil,
			wantUser:   nil,
			wantLevels: nil,
			wantOK:     false,
		},
		{
			name:       "single rating not",
			expr:       NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
			wantUser:   nil,
			wantLevels: []string{"explicit"},
			wantOK:     true,
		},
		{
			name: "pure chain",
			expr: AndExpr{
				Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
				Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
			},
			wantUser:   nil,
			wantLevels: []string{"questionable", "explicit"},
			wantOK:     true,
		},
		{
			name: "wrapped tag",
			expr: AndExpr{
				Left:  TagExpr{Tag: "blue_eyes"},
				Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
			},
			wantUser:   TagExpr{Tag: "blue_eyes"},
			wantLevels: []string{"explicit"},
			wantOK:     true,
		},
		{
			name: "no rating predicates",
			expr: AndExpr{
				Left:  TagExpr{Tag: "a"},
				Right: TagExpr{Tag: "b"},
			},
			wantUser:   nil,
			wantLevels: nil,
			wantOK:     false,
		},
		{
			name:       "or-wrapped chain stays as user",
			expr:       OrExpr{Left: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}}, Right: TagExpr{Tag: "a"}},
			wantUser:   nil,
			wantLevels: nil,
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, levels, ok := extractCeilingShape(tt.expr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !exprEqual(user, tt.wantUser) {
				t.Errorf("user = %+v, want %+v", user, tt.wantUser)
			}
			if len(levels) != len(tt.wantLevels) {
				t.Fatalf("levels = %v, want %v", levels, tt.wantLevels)
			}
			for i := range levels {
				if levels[i] != tt.wantLevels[i] {
					t.Errorf("levels[%d] = %q, want %q", i, levels[i], tt.wantLevels[i])
				}
			}
		})
	}
}

func exprEqual(a, b Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func TestIsPureTagExpr(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
		want bool
	}{
		{"literal tag", TagExpr{Tag: "1girl"}, true},
		{"prefix tag", TagExpr{Tag: "blue", Wildcard: "prefix"}, true},
		{"substring tag", TagExpr{Tag: "blue", Wildcard: "substring"}, true},
		{"and of tags", AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"or of tags", OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"not tag", NotExpr{Expr: TagExpr{Tag: "a"}}, true},
		{"cat: filter", FilterExpr{Key: "cat", Val: "character"}, true},
		{"tagged: filter", FilterExpr{Key: "tagged", Val: "true"}, true},
		{"autotagged: filter", FilterExpr{Key: "autotagged", Val: "true"}, true},
		{"category-qualified (unknown key)", FilterExpr{Key: "character", Val: "miku"}, true},
		{"colon tag fallback (unknown key)", FilterExpr{Key: "nier", Val: "automata"}, true},
		{"fav filter", FilterExpr{Key: "fav", Val: "true"}, false},
		{"ai filter", FilterExpr{Key: "ai", Val: "any"}, false},
		{"source exact filter", FilterExpr{Key: "source", Val: "Pixiv"}, false},
		{"folder filter", FilterExpr{Key: "folder", Val: "anime"}, false},
		{"file_size filter", FilterExpr{Key: "size", Val: ">=10MB"}, false},
		{"mime filter", FilterExpr{Key: "mime", Val: "png"}, false},
		{"width filter", FilterExpr{Key: "width", Val: ">=1024"}, true},
		{"height filter", FilterExpr{Key: "height", Val: ">=1024"}, true},
		{"date filter", FilterExpr{Key: "date", Val: "2024"}, true},
		{"ratio filter", FilterExpr{Key: "ratio", Val: ">=1.5"}, true},
		{"duration filter", FilterExpr{Key: "duration", Val: ">=30"}, false},
		{"name filter", FilterExpr{Key: "name", Val: "img"}, false},
		{"prompt filter", FilterExpr{Key: "prompt", Val: "masterpiece"}, false},
		{"seed filter", FilterExpr{Key: "seed", Val: "42"}, false},
		{"tagcount filter", FilterExpr{Key: "tagcount", Val: ">=5"}, true},
		{"tag and fav", AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureTagExpr(tt.expr); got != tt.want {
				t.Errorf("isPureTagExpr(%+v) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestFastTagTotal_NonexistentTag(t *testing.T) {
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, TagExpr{Tag: "no_such_tag"})
	if !ok {
		t.Fatal("nonexistent tag should resolve as confirmed-empty (ok=true)")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_SingleCanonical(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('hot', ?, 549514)`,
		generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "hot"})
	if !ok {
		t.Fatal("single-canonical tag should hit fast path (ok=true)")
	}
	if n != 549514 {
		t.Errorf("count = %d, want 549514", n)
	}
}

func TestFastTagTotal_AliasFollowsCanonical(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('feline', ?, 7) RETURNING id`,
		generalID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('cat', ?, 1, ?)`,
		generalID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "cat"})
	if !ok {
		t.Fatal("alias should resolve via canonical (ok=true)")
	}
	if n != 7 {
		t.Errorf("count = %d, want 7 (canonical's usage_count)", n)
	}
}

func TestFastTagTotal_MultipleCanonicalsFallthrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID, charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cat', ?, 3)`, generalID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cat', ?, 5)`, charID,
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := fastTagTotal(database, TagExpr{Tag: "cat"}); ok {
		t.Error("multi-canonical name must fall through to slow path (ok=false)")
	}
}

func TestFastTagTotal_WildcardPrefix(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	// Usages above fastApproxThreshold so the upper-bound short-circuit
	// engages instead of falling through to the slow exact COUNT.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes',  ?, 40000),
		    ('blue_hair',  ?, 20000),
		    ('green_eyes', ?, 30)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard prefix should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum over name LIKE 'blue%%')", n)
	}
}

func TestFastTagTotal_WildcardSubstring(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes',  ?, 40000),
		    ('light_blue', ?, 20000),
		    ('green_eyes', ?, 30)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "substring"})
	if !ok {
		t.Fatal("wildcard substring should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum over name LIKE '%%blue%%')", n)
	}
}

func TestFastTagTotal_WildcardSuffix(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes',  ?, 40000),
		    ('green_eyes', ?, 20000),
		    ('blue_hair',  ?, 30)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "_eyes", Wildcard: "suffix"})
	if !ok {
		t.Fatal("wildcard suffix should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum over name LIKE '%%_eyes')", n)
	}
}

func TestFastTagTotal_WildcardBelowThresholdFallsThrough(t *testing.T) {
	// Multi-canonical wildcard with small usages: the slow path is fast
	// and exact, so the fast path bails to keep displayed totals exact.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes', ?, 5),
		    ('blue_hair', ?, 3)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"}); ok {
		t.Error("sub-threshold wildcard should fall through to the exact slow path")
	}
}

func TestFastTagTotal_WildcardCollapsesAlias(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blueberry', ?, 7) RETURNING id`,
		generalID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('bluebell', ?, 1, ?)`,
		generalID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with alias should hit fast path")
	}
	if n != 7 {
		t.Errorf("count = %d, want 7 (alias and canonical collapse via DISTINCT COALESCE)", n)
	}
}

func TestFastTagTotal_WildcardEmpty(t *testing.T) {
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, TagExpr{Tag: "nomatch_zzzzz", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with no matches should resolve as confirmed-empty")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_WildcardEscapesMetacharacters(t *testing.T) {
	// A wildcard like `foo_*` must match `foo_bar` literally (the underscore
	// is part of the tag name) and NOT every name with any character at
	// position 4. escapeLike + ESCAPE '\' carries that through.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('foo_bar',  ?, 10),
		    ('fooXbar', ?, 99)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "foo_b", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with literal underscore should hit fast path")
	}
	if n != 10 {
		t.Errorf("count = %d, want 10 (only foo_bar, not fooXbar)", n)
	}
}

func TestFastTagTotal_RejectsNonRecognisedShapes(t *testing.T) {
	database, _ := setupSearchDB(t)
	// Filter keywords with their own selective indexes still fall through.
	// (`folder` and `source` now have fast counts of their own; `tagged`,
	// `autotagged`, `rating`, `generated` were already handled.)
	for _, key := range []string{"fav", "width", "date"} {
		if _, ok := fastTagTotal(database, FilterExpr{Key: key, Val: "true"}); ok {
			t.Errorf("FilterExpr{%q} should fall through to slow path", key)
		}
	}
	// AND/OR with a non-fast-pathable leaf falls through.
	mixed := AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(database, mixed); ok {
		t.Error("AND with FilterExpr{fav} leaf should fall through")
	}
	mixedOr := OrExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(database, mixedOr); ok {
		t.Error("OR with FilterExpr{fav} leaf should fall through")
	}
	// NotExpr with non-literal inner falls through.
	if _, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "blue", Wildcard: "prefix"}}); ok {
		t.Error("NOT with wildcard inner should fall through")
	}
	if _, ok := fastTagTotal(database, NotExpr{Expr: FilterExpr{Key: "fav", Val: "true"}}); ok {
		t.Error("NOT with non-tag inner should fall through")
	}
}

func TestFastTagTotal_NotSingleTag(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "n_a.png")
	ingestTestImage(t, database, env, "n_b.png")
	ingestTestImage(t, database, env, "n_c.png")

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('hot', ?, 2)`, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "hot"}})
	if !ok {
		t.Fatal("NOT single tag should hit fast path")
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (3 visible - 2 hot)", n)
	}
}

func TestFastTagTotal_NotMissingTag(t *testing.T) {
	// NotExpr{tag} where the tag doesn't exist should report all visible.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "nm_a.png")
	ingestTestImage(t, database, env, "nm_b.png")

	n, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "no_such_tag"}})
	if !ok {
		t.Fatal("NOT of missing tag should hit fast path")
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (all visible)", n)
	}
}

func TestFastTagTotal_AndPositiveTags(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	// Each tag's usage above fastApproxThreshold so min(...) clears the
	// gate and the upper-bound short-circuit engages.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('a', ?, 100000),
		    ('b', ?, 60000),
		    ('c', ?, 200000)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := AndExpr{
		Left: TagExpr{Tag: "a"},
		Right: AndExpr{
			Left:  TagExpr{Tag: "b"},
			Right: TagExpr{Tag: "c"},
		},
	}
	n, ok := fastTagTotal(database, expr)
	if !ok {
		t.Fatal("AND of high-usage positive tags should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (min upper bound)", n)
	}
}

func TestFastTagTotal_AndBelowThresholdFallsThrough(t *testing.T) {
	// Small AND queries fall through to the exact slow COUNT so totals
	// like `cute dog` (no overlap → 0) stay exact in tests and small
	// libraries.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cute', ?, 3), ('dog', ?, 2)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := AndExpr{Left: TagExpr{Tag: "cute"}, Right: TagExpr{Tag: "dog"}}
	if _, ok := fastTagTotal(database, expr); ok {
		t.Error("sub-threshold AND should fall through to the exact slow path")
	}
}

func TestFastTagTotal_OrCappedAtVisibleCount(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "or_cap.png") // 1 visible image total

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	// Drift-style usage_counts above what the (1) visible image
	// supports; the cap should clamp the upper bound to visible_count.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('a', ?, 30000), ('b', ?, 30000)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}
	n, ok := fastTagTotal(database, expr)
	if !ok {
		t.Fatal("OR fast path expected")
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (sum=60000 capped at visible=1)", n)
	}
}

func TestFastTagTotal_OrBelowThresholdFallsThrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('a', ?, 5), ('b', ?, 3)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}
	expr := OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}
	if _, ok := fastTagTotal(database, expr); ok {
		t.Error("sub-threshold OR should fall through to the exact slow path")
	}
}

func TestFastTagTotal_CatFilter(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID, charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	// Sum within character above fastApproxThreshold so the upper-bound
	// short-circuit engages.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('miku',  ?, 40000),
		    ('haku',  ?, 20000),
		    ('1girl', ?, 100000)`,
		charID, charID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "cat", Val: "character"})
	if !ok {
		t.Fatal("cat: filter should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum within character, general excluded)", n)
	}
}

func TestFastTagTotal_CatFilterBelowThresholdFallsThrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('miku', ?, 5), ('haku', ?, 3)`,
		charID, charID,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := fastTagTotal(database, FilterExpr{Key: "cat", Val: "character"}); ok {
		t.Error("sub-threshold cat: should fall through to the exact slow path")
	}
}

func TestFastTagTotal_CategoryQualifiedTag(t *testing.T) {
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('miku', ?, 5)`, charID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "miku"})
	if !ok {
		t.Fatal("character:miku should hit fast path")
	}
	if n != 5 {
		t.Errorf("count = %d, want 5", n)
	}
}

func TestFastTagTotal_CategoryQualifiedFollowsAlias(t *testing.T) {
	// character:cat aliased to character:feline - querying the alias
	// should report the canonical's usage_count.
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('feline', ?, 9) RETURNING id`,
		charID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('cat', ?, 1, ?)`,
		charID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "cat"})
	if !ok {
		t.Fatal("alias should resolve via canonical")
	}
	if n != 9 {
		t.Errorf("count = %d, want 9 (canonical's usage_count)", n)
	}
}

func TestFastTagTotal_CategoryQualifiedMissingTag(t *testing.T) {
	// (name, cat) pair doesn't exist but the category does - exact 0.
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "no_such_char"})
	if !ok {
		t.Fatal("missing tag in real category should still hit fast path")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_CategoryQualifiedUnknownCategory(t *testing.T) {
	// "nier:automata" - "nier" is not a real category. Slow path falls
	// back to a literal-tag search; fast path bails so that runs.
	database, _ := setupSearchDB(t)
	if _, ok := fastTagTotal(database, FilterExpr{Key: "nier", Val: "automata"}); ok {
		t.Error("unknown category must fall through")
	}
}

func TestExecute_FastTagTotal_EmptyShortCircuits(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fast_empty.png")

	result, err := Execute(database, Query{
		Expr:  TagExpr{Tag: "tag_that_does_not_exist"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 || len(result.Results) != 0 {
		t.Errorf("Total = %d, Results = %d, want both 0", result.Total, len(result.Results))
	}
}

func TestExecute_AndDriverMultiTag(t *testing.T) {
	// 3-AND of literal tags: the AND-driver replaces the smallest tag's
	// correlated EXISTS with a non-correlated IN subquery. The set of
	// matching images must be unchanged from the slow path.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "and_match.png")
	ingestTestImage(t, database, env, "and_pair_only.png")
	ingestTestImage(t, database, env, "and_single.png")

	var matchID, pairID, singleID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%and_match.png'`).Scan(&matchID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%and_pair_only.png'`).Scan(&pairID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%and_single.png'`).Scan(&singleID)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	mkTag := func(name string) int64 {
		var id int64
		database.Write.QueryRow(
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, name, generalID,
		).Scan(&id)
		return id
	}
	tagA := mkTag("alpha")
	tagB := mkTag("bravo")
	tagC := mkTag("charlie")

	// match: A,B,C ; pair: A,B ; single: A. Smallest carrier set is C
	// (one image), so the driver should pick C and the slow EXISTS for
	// A and B run on that singleton.
	attachTag(t, database, matchID, tagA)
	attachTag(t, database, matchID, tagB)
	attachTag(t, database, matchID, tagC)
	attachTag(t, database, pairID, tagA)
	attachTag(t, database, pairID, tagB)
	attachTag(t, database, singleID, tagA)

	expr := AndExpr{
		Left:  AndExpr{Left: TagExpr{Tag: "alpha"}, Right: TagExpr{Tag: "bravo"}},
		Right: TagExpr{Tag: "charlie"},
	}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1", result.Total)
	}
	if len(result.Results) != 1 || result.Results[0].ID != matchID {
		t.Errorf("matched = %v, want only %d", result.Results, matchID)
	}
}

func TestExecute_AndDriverPreservesOrAndNot(t *testing.T) {
	// The driver only fires on AND-only paths from the root. A literal
	// tag inside OR or NOT must keep its correlated EXISTS so semantics
	// stay intact.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "or_a.png")
	ingestTestImage(t, database, env, "or_b.png")
	ingestTestImage(t, database, env, "or_c.png")

	var idA, idB, idC int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%or_a.png'`).Scan(&idA)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%or_b.png'`).Scan(&idB)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%or_c.png'`).Scan(&idC)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	mkTag := func(name string) int64 {
		var id int64
		database.Write.QueryRow(
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, name, generalID,
		).Scan(&id)
		return id
	}
	tagX := mkTag("xray")
	tagY := mkTag("yankee")

	attachTag(t, database, idA, tagX)
	attachTag(t, database, idB, tagY)
	// idC has neither.

	// X OR Y matches A and B only. Driver must not fire because the
	// leaves live under OrExpr.
	expr := OrExpr{Left: TagExpr{Tag: "xray"}, Right: TagExpr{Tag: "yankee"}}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
}

// TestPickAndDriverTag_SingleWildcard pins the wildcard-only branch of
// the driver: a single prefix TagExpr at root (the F206 shape: the
// detail page's random-sort adjacency rides this expression) gets the
// driver, replacing the LIST SUBQUERY scan with a literal IN(...).
func TestPickAndDriverTag_SingleWildcard(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes', ?, 12),
		    ('blue_hair', ?, 7),
		    ('red_hair',  ?, 5)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	legs, ok := pickAndDriverTag(database, TagExpr{Tag: "blue", Wildcard: "prefix"}, false)
	if !ok {
		t.Fatal("single wildcard should engage the driver")
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1 single-leaf path", len(legs))
	}
	if legs[0].leaf.Tag != "blue" || legs[0].leaf.Wildcard != "prefix" {
		t.Errorf("driver leaf = %+v, want {blue, prefix}", legs[0].leaf)
	}
	if len(legs[0].ids) != 2 {
		t.Errorf("driver canonicals = %d, want 2 (blue_eyes + blue_hair)", len(legs[0].ids))
	}
}

func TestPickAndDriverTag_SingleLiteralStillSkips(t *testing.T) {
	// A single literal at root is one EXISTS the planner already
	// handles well; the driver isn't useful there. Keeps the existing
	// behaviour from before the wildcard generalisation.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('alpha',   ?, 5),
		    ('popular', ?, ?)`,
		generalID,
		generalID, andDriverThreshold+10,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, false); ok {
		t.Error("single literal at root should not engage the driver under indexed sort")
	}
	// Random sort is the override: single literal does engage so the
	// materialised set bounds the temp-sort input. Both the cheap
	// (alpha) and the popular leaf (above andDriverThreshold) have to
	// engage; under random sort the slow path TEMP-B-TREEs every
	// visible carrier and any candidate-set bound is profitable
	// regardless of usage.
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, true); !ok {
		t.Error("single literal at root should engage the driver under random sort")
	}
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "popular"}, true); !ok {
		t.Error("popular single literal under random sort should still engage the driver")
	}
}

// TestExecute_RandomSortSingleTagDriver pins the path where a random-sort
// query with a single literal tag predicate engages the AND-driver so the
// random-key TEMP B-TREE sort runs against the bounded image set
// rather than every visible image carrying the predicate.
func TestExecute_RandomSortSingleTagDriver(t *testing.T) {
	database, env := setupSearchDB(t)
	for _, name := range []string{"r_blue1.png", "r_blue2.png", "r_red.png"} {
		ingestTestImage(t, database, env, name)
	}
	var blue1, blue2, red int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%r_blue1.png'`).Scan(&blue1)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%r_blue2.png'`).Scan(&blue2)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%r_red.png'`).Scan(&red)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	var blueID, redID int64
	database.Write.QueryRow(`INSERT INTO tags (name, category_id) VALUES ('blue_eyes', ?) RETURNING id`, generalID).Scan(&blueID)
	database.Write.QueryRow(`INSERT INTO tags (name, category_id) VALUES ('red_eyes', ?) RETURNING id`, generalID).Scan(&redID)
	attachTag(t, database, blue1, blueID)
	attachTag(t, database, blue2, blueID)
	attachTag(t, database, red, redID)

	result, err := Execute(database, Query{
		Expr:       TagExpr{Tag: "blue_eyes"},
		Sort:       "random",
		RandomSeed: 1234567890,
		Page:       1,
		Limit:      40,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The matching set is exactly {blue1, blue2}; ordering is the
	// random-seed permutation but membership is what we pin.
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	got := map[int64]bool{}
	for _, img := range result.Results {
		got[img.ID] = true
	}
	if !got[blue1] || !got[blue2] || got[red] {
		t.Errorf("results = %+v, want exactly {blue1, blue2}", got)
	}
}

// TestPickAndDriverTag_PopularIntersect pins the popular-AND path: every
// leaf of an AND chain has usage_count above andDriverThreshold, so the
// driver returns the two least-popular legs to INTERSECT-bound the
// candidate set. Remaining leaves keep their correlated EXISTS.
// The single-leg shape (smallest below threshold) keeps its old contract.
func TestPickAndDriverTag_PopularIntersect(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('a_pop',  ?, ?),
		    ('b_pop',  ?, ?),
		    ('c_pop',  ?, ?),
		    ('d_rare', ?, 5)`,
		generalID, andDriverThreshold+10,
		generalID, andDriverThreshold+20,
		generalID, andDriverThreshold+30,
		generalID,
	); err != nil {
		t.Fatal(err)
	}

	// Three popular leaves AND'd: every leg above threshold, the two
	// least-popular (a_pop, b_pop) feed the INTERSECT; c_pop falls
	// through to its correlated EXISTS over the bounded candidate set.
	popular := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "a_pop"},
			Right: TagExpr{Tag: "b_pop"},
		},
		Right: TagExpr{Tag: "c_pop"},
	}
	legs, ok := pickAndDriverTag(database, popular, false)
	if !ok {
		t.Fatal("popular 3-AND should engage the driver via INTERSECT")
	}
	if len(legs) != 2 {
		t.Errorf("legs = %d, want 2 (cap at the two least-popular leaves)", len(legs))
	}
	picked := map[string]bool{}
	for _, l := range legs {
		picked[l.leaf.Tag] = true
	}
	if !picked["a_pop"] || !picked["b_pop"] {
		t.Errorf("picked = %v, want {a_pop, b_pop} (least-popular pair)", picked)
	}
	if picked["c_pop"] {
		t.Errorf("c_pop is the most popular leaf and should fall through to correlated EXISTS")
	}

	// Same chain plus the rare leaf: smallest is below threshold so
	// the driver picks the single-leg path against d_rare.
	mixed := AndExpr{
		Left:  popular,
		Right: TagExpr{Tag: "d_rare"},
	}
	legs2, ok := pickAndDriverTag(database, mixed, false)
	if !ok {
		t.Fatal("rare-tag-wins shape should engage the single-leg driver")
	}
	if len(legs2) != 1 {
		t.Fatalf("legs = %d, want 1 single-leg path on the rare leaf", len(legs2))
	}
	if legs2[0].leaf.Tag != "d_rare" {
		t.Errorf("driver leaf = %q, want d_rare", legs2[0].leaf.Tag)
	}
}

// TestApplyAndDriver_IntersectSQL pins the SQL shape applyAndDriver
// emits for multi-leg legs: each leg's image_id stream is INTERSECTed
// inside the i.id IN (...) wrap.
func TestApplyAndDriver_IntersectSQL(t *testing.T) {
	legs := []andDriverLeg{
		{leaf: TagExpr{Tag: "a"}, ids: []int64{1, 2}},
		{leaf: TagExpr{Tag: "b"}, ids: []int64{3, 4, 5}},
	}
	where, args := applyAndDriver("", nil, legs)
	wantPrefix := "i.id IN (SELECT image_id FROM image_tags WHERE tag_id IN (?,?) INTERSECT SELECT image_id FROM image_tags WHERE tag_id IN (?,?,?))"
	if where != wantPrefix {
		t.Errorf("multi-leg SQL = %q, want %q", where, wantPrefix)
	}
	if len(args) != 5 {
		t.Errorf("args len = %d, want 5", len(args))
	}

	// Single leg keeps the simpler IN-only shape, no INTERSECT keyword.
	single := []andDriverLeg{{leaf: TagExpr{Tag: "a"}, ids: []int64{1, 2}}}
	whereSingle, _ := applyAndDriver("", nil, single)
	if strings.Contains(whereSingle, "INTERSECT") {
		t.Errorf("single-leg SQL should not use INTERSECT, got %q", whereSingle)
	}
}

// TestExecute_PopularAndIntersect runs the popular-3-AND end-to-end,
// asserting the INTERSECT driver matches the slow-path's exact result
// set on a small fixture (the slow path falls through this path for
// usage > threshold; here we set the threshold via tag-row usage_count
// so the path is exercised on test-sized data).
func TestExecute_PopularAndIntersect(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "all_three.png")
	ingestTestImage(t, database, env, "ab_only.png")
	ingestTestImage(t, database, env, "a_only.png")

	var allID, abID, aID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%all_three.png'`).Scan(&allID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%ab_only.png'`).Scan(&abID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%a_only.png'`).Scan(&aID)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	mkTag := func(name string, usage int) int64 {
		var id int64
		database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?) RETURNING id`,
			name, generalID, usage,
		).Scan(&id)
		return id
	}
	// Force every leaf above andDriverThreshold so the INTERSECT path
	// fires on this small fixture.
	a := mkTag("a_int", andDriverThreshold+1)
	b := mkTag("b_int", andDriverThreshold+1)
	c := mkTag("c_int", andDriverThreshold+1)
	for _, p := range []struct {
		img int64
		tag int64
	}{
		{allID, a}, {allID, b}, {allID, c},
		{abID, a}, {abID, b},
		{aID, a},
	} {
		attachTag(t, database, p.img, p.tag)
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "a_int"},
			Right: TagExpr{Tag: "b_int"},
		},
		Right: TagExpr{Tag: "c_int"},
	}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].ID != allID {
		t.Errorf("Results = %+v, want only %d (all_three)", result.Results, allID)
	}
}

// TestExecute_RecentIDBoundSkipsAsc pins the multi-leg INTERSECT
// recent-id bound's DESC-only scope. The bound predicate
// `image_id >= bound` covers the recent-newest id range; under
// `order=asc` the user asked for the oldest matches, whose ids sit
// below the bound. Firing the gate there used to silently drop them
// and surface the asc-by-ingested-at slice of the recent end instead.
func TestExecute_RecentIDBoundSkipsAsc(t *testing.T) {
	database, _ := setupSearchDB(t)

	// Need more than (page * limit * driverIDBoundMargin) = 100 visible
	// images so the bound's `LIMIT 1 OFFSET targetOffset` returns a row;
	// 150 puts the bound mid-range so a buggy gate would drop the oldest
	// match. A recursive CTE inserts in one statement.
	if _, err := database.Write.Exec(`
		WITH RECURSIVE seq(n) AS (
		    SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 150
		)
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		SELECT '/tmp/asc_' || n || '.png', 'png', 1024, 'asc_sha_' || n, datetime('now')
		FROM seq
	`); err != nil {
		t.Fatal(err)
	}

	var oldestID, newestID int64
	if err := database.Read.QueryRow(
		`SELECT MIN(id), MAX(id) FROM images`,
	).Scan(&oldestID, &newestID); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	mkTag := func(name string, usage int) int64 {
		var id int64
		database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?) RETURNING id`,
			name, generalID, usage,
		).Scan(&id)
		return id
	}
	// Force both leaves above andDriverThreshold so pickAndDriverTag picks
	// the two-leg INTERSECT path; that's the only path that ever attaches
	// the recent-id bound to a leg.
	lo := mkTag("lo_tag", andDriverThreshold+1)
	hi := mkTag("hi_tag", andDriverThreshold+1)
	for _, id := range []int64{oldestID, newestID} {
		attachTag(t, database, id, lo)
		attachTag(t, database, id, hi)
	}

	res, err := Execute(database, Query{
		Expr: AndExpr{
			Left:  TagExpr{Tag: "lo_tag"},
			Right: TagExpr{Tag: "hi_tag"},
		},
		Sort:  "newest",
		Order: "asc",
		Page:  1,
		Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].ID != oldestID {
		t.Errorf("Results = %+v, want oldest carrier id=%d (asc order)", res.Results, oldestID)
	}
}

// TestExecute_AndDriverWildcardReplacesListSubquery runs the F206 shape
// end-to-end: a single wildcard predicate. Without the wildcard driver
// the EXISTS body rides a LIST SUBQUERY scan of every tag matching
// `blue%`. Asserting on results verifies the substitution preserves
// semantics.
func TestExecute_AndDriverWildcardReplacesListSubquery(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "wd_blue1.png")
	ingestTestImage(t, database, env, "wd_blue2.png")
	ingestTestImage(t, database, env, "wd_other.png")
	ingestTestImage(t, database, env, "wd_none.png")

	var blue1ID, blue2ID, otherID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wd_blue1.png'`).Scan(&blue1ID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wd_blue2.png'`).Scan(&blue2ID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%wd_other.png'`).Scan(&otherID)

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	mkTag := func(name string) int64 {
		var id int64
		database.Write.QueryRow(
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, name, generalID,
		).Scan(&id)
		return id
	}
	blueEyes := mkTag("blue_eyes")
	blueHair := mkTag("blue_hair")
	redHair := mkTag("red_hair")
	attachTag(t, database, blue1ID, blueEyes)
	attachTag(t, database, blue2ID, blueHair)
	attachTag(t, database, otherID, redHair)

	result, err := Execute(database, Query{
		Expr:  TagExpr{Tag: "blue", Wildcard: "prefix"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2 (blue_eyes + blue_hair carriers)", result.Total)
	}
	matched := map[int64]bool{}
	for _, img := range result.Results {
		matched[img.ID] = true
	}
	if !matched[blue1ID] || !matched[blue2ID] {
		t.Errorf("results missing blue carriers: %v", result.Results)
	}
	if matched[otherID] {
		t.Errorf("non-blue image %d leaked into results", otherID)
	}
}

func TestExecute_RatingUnusedShortCircuits(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rating_unused.png")

	// No image carries any rating tag. The positive form must return zero
	// matches without paying the full image scan that the EXISTS predicate
	// would otherwise force on the LIMIT-bounded data path.
	result, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "rating", Val: "explicit"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 || len(result.Results) != 0 {
		t.Errorf("Total = %d, Results = %d, want both 0", result.Total, len(result.Results))
	}
}

func TestExecute_FullSync(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	gallery.Sync(context.Background(), database, env.galleryDir, env.thumbnailsDir, env.maxFileSizeMB, func(int, int, string) {})

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

func TestBuildOrder_Newest(t *testing.T) {
	got := buildOrder("newest", "", 0)
	if !strings.Contains(got, "DESC") || !strings.Contains(got, "ingested_at") {
		t.Errorf("newest default: %q", got)
	}
}

func TestBuildOrder_NewestAsc(t *testing.T) {
	got := buildOrder("newest", "asc", 0)
	if !strings.Contains(got, "ASC") {
		t.Errorf("newest asc: %q", got)
	}
}

func TestBuildOrder_Filesize(t *testing.T) {
	got := buildOrder("filesize", "", 0)
	if !strings.Contains(got, "file_size") || !strings.Contains(got, "DESC") {
		t.Errorf("filesize: %q", got)
	}
}

func TestBuildOrder_FilesizeAsc(t *testing.T) {
	got := buildOrder("filesize", "asc", 0)
	if !strings.Contains(got, "file_size") || !strings.Contains(got, "ASC") {
		t.Errorf("filesize asc: %q", got)
	}
}

func TestBuildOrder_Order(t *testing.T) {
	got := buildOrder("order", "", 0)
	if !strings.Contains(got, "i.series ASC") || !strings.Contains(got, "i.series_order IS NULL") || !strings.Contains(got, "i.series_order ASC") || !strings.Contains(got, "i.id ASC") {
		t.Errorf("order default: %q", got)
	}
}

func TestBuildOrder_OrderDesc(t *testing.T) {
	got := buildOrder("order", "desc", 0)
	if !strings.Contains(got, "i.series DESC") || !strings.Contains(got, "i.series_order IS NULL") || !strings.Contains(got, "i.series_order DESC") || !strings.Contains(got, "i.id DESC") {
		t.Errorf("order desc: %q", got)
	}
}

func TestBuildOrder_Unknown(t *testing.T) {
	// Unknown sorts fall back to newest (ingested_at DESC)
	got := buildOrder("unknown_sort", "", 0)
	if !strings.Contains(got, "ingested_at") || !strings.Contains(got, "DESC") {
		t.Errorf("unknown sort: %q", got)
	}
}

func TestBuildOrder_Random(t *testing.T) {
	// The seed is mixed before interpolation so the multiplier the
	// SQL sees is mixSeed(seed), not the raw input.
	got := buildOrder("random", "", 12345)
	mixed := mixSeed(12345)
	if !strings.Contains(got, fmt.Sprintf("%d", mixed)) {
		t.Errorf("random: expected mixed seed %d in order clause, got %q", mixed, got)
	}
}

func TestMixSeed_StableForSameInput(t *testing.T) {
	if mixSeed(1) != mixSeed(1) {
		t.Error("mixSeed must be deterministic for the same input")
	}
}

func TestMixSeed_OddAndHighBitForSmallSeeds(t *testing.T) {
	// Every non-zero seed must produce an odd value with the 2^30 bit
	// set so `(id * mixed) & 2^31-1` is a permutation that overflows
	// uint32 for any plausible image id.
	for _, in := range []int64{1, 7, 100, 1000, 12345, 9999999} {
		out := mixSeed(in)
		if out&1 == 0 {
			t.Errorf("mixSeed(%d) = %d is even; the modular product is not a permutation", in, out)
		}
		if out < (1 << 30) {
			t.Errorf("mixSeed(%d) = %d < 2^30; small seeds would still produce identity order", in, out)
		}
		if out >= (1 << 31) {
			t.Errorf("mixSeed(%d) = %d >= 2^31; multiplication can overflow int64 at large image ids", in, out)
		}
	}
}

func TestBuildWhere_TagExact(t *testing.T) {
	expr := TagExpr{Tag: "cute", Wildcard: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "cute" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "name = ?") {
		t.Errorf("where = %q", where)
	}
	// Alias-aware: the tag_id lookup goes through COALESCE so a name
	// matching an alias row resolves to its canonical.
	if !strings.Contains(where, "COALESCE(canonical_tag_id, id)") {
		t.Errorf("where missing alias resolution: %q", where)
	}
}

func TestBuildWhere_TagPrefix(t *testing.T) {
	expr := TagExpr{Tag: "blue", Wildcard: "prefix"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "blue%" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "LIKE ?") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_TagSubstring(t *testing.T) {
	expr := TagExpr{Tag: "hair", Wildcard: "substring"}
	_, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "%hair%" {
		t.Errorf("args = %v", args)
	}
}

func TestBuildWhere_FavFalse(t *testing.T) {
	expr := FilterExpr{Key: "fav", Val: "false"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for fav:false")
	}
	if !strings.Contains(where, "is_favorited = 0") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_InboxTrue(t *testing.T) {
	expr := FilterExpr{Key: "inbox", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for inbox:true")
	}
	if !strings.Contains(where, "is_inbox = 1") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_InboxFalse(t *testing.T) {
	expr := FilterExpr{Key: "inbox", Val: "false"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for inbox:false")
	}
	if !strings.Contains(where, "is_inbox = 0") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_AI(t *testing.T) {
	// "sd" is aliased to "a1111"; ai filter uses 4 LIKE args for comma-separated source_type values.
	expr := FilterExpr{Key: "ai", Val: "sd"}
	where, args, _ := buildWhere(expr)
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
	if len(args) > 0 && args[0] != "a1111" {
		t.Errorf("args[0] = %v, want a1111", args[0])
	}
	if !strings.Contains(where, "source_type") {
		t.Errorf("where = %q, expected source_type", where)
	}
}

func TestBuildWhere_AIAny(t *testing.T) {
	// "any" expands inline to match a1111, comfyui, and the combined source type;
	// no bound args are needed because the values are inlined into the SQL.
	expr := FilterExpr{Key: "ai", Val: "any"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for ai:any, got %v", args)
	}
	for _, want := range []string{"'a1111'", "'comfyui'", "'a1111,comfyui'"} {
		if !strings.Contains(where, want) {
			t.Errorf("where = %q, missing %s", where, want)
		}
	}
}

func TestBuildWhere_AINone(t *testing.T) {
	// "none" collapses to a single source_type equality so the partial
	// idx_images_source_type_visible can answer the seek directly,
	// rather than the four-LIKE shape that drove the planner past
	// idx_images_source_type onto idx_images_missing.
	expr := FilterExpr{Key: "ai", Val: "none"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for ai:none, got %v", args)
	}
	if !strings.Contains(where, "i.source_type = 'none'") {
		t.Errorf("where = %q, want a bare source_type='none' equality", where)
	}
	if strings.Contains(where, "LIKE") {
		t.Errorf("where = %q, want no LIKE - none is never combined", where)
	}
}

func TestBuildWhere_SourceExact(t *testing.T) {
	// `source:` is a separate exact-match filter against images.source.
	expr := FilterExpr{Key: "source", Val: "Pixiv"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "Pixiv" {
		t.Errorf("args = %v, want [\"Pixiv\"]", args)
	}
	if !strings.Contains(where, "i.source = ?") {
		t.Errorf("where = %q, expected i.source = ?", where)
	}
}

func TestBuildWhere_Cat(t *testing.T) {
	expr := FilterExpr{Key: "cat", Val: "character"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "character" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "tc.name = ?") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Width(t *testing.T) {
	expr := FilterExpr{Key: "width", Val: ">=1920"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != int64(1920) {
		t.Errorf("args = %v, want int64(1920)", args)
	}
	if !strings.Contains(where, "i.width") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Height(t *testing.T) {
	expr := FilterExpr{Key: "height", Val: "<768"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != int64(768) {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "i.height") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_WidthNonNumericRejected(t *testing.T) {
	// A non-numeric width comparand must produce `1=0`, not bind the string
	// into the SQL - SQLite would coerce it to 0 and match every row with
	// width >= 0, which is worse than returning nothing.
	expr := FilterExpr{Key: "width", Val: ">=abc"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, expected 1=0", where)
	}
}

func TestBuildWhere_MissingFalse(t *testing.T) {
	// `missing:false` (and `missing:true`) opt out of the auto-injected
	// `AND is_missing = 0`; the explicit clause speaks for itself, and
	// without the opt-out a negation like `-missing:false` would
	// collapse to a contradiction and match nothing.
	expr := FilterExpr{Key: "missing", Val: "false"}
	where, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true for missing:false")
	}
	if !strings.Contains(where, "is_missing = 0") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_NegatedMissingFalse(t *testing.T) {
	// `-missing:false` should mean "show me missing images", equivalent to
	// `missing:true`. The auto-injected `AND is_missing = 0` clause must
	// not be layered on top of the negation, or the query collapses to
	// zero results.
	expr := NotExpr{Expr: FilterExpr{Key: "missing", Val: "false"}}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT") {
		t.Errorf("where missing NOT: %q", where)
	}
	// The auto-injection must NOT have been appended; otherwise the
	// caller (Execute) would build a contradictory clause.
	if strings.Contains(where, "AND i.is_missing = 0") {
		t.Errorf("where should not include the auto-clause: %q", where)
	}
}

func TestBuildWhere_Name(t *testing.T) {
	// name: rides the indexed basename_lower VIRTUAL column so the
	// match is scoped to the filename segment without paying the
	// per-row basename() function call. The value is lowercased on
	// both sides to keep the search case-insensitive end-to-end, and
	// the same pattern is bound twice: once for the canonical row's
	// basename_lower and once for the alias-path EXISTS so SHA-256
	// duplicates show up under either filename.
	expr := FilterExpr{Key: "name", Val: "Vacation"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "i.basename_lower LIKE") {
		t.Errorf("where = %q, want basename_lower LIKE", where)
	}
	if !strings.Contains(where, "FROM image_paths ip") {
		t.Errorf("where = %q, want an alias-path EXISTS", where)
	}
	if len(args) != 2 {
		t.Fatalf("args = %v, want canonical+alias pattern args", args)
	}
	for _, a := range args {
		got, _ := a.(string)
		if got != "%vacation%" {
			t.Errorf("pattern = %q, want %%vacation%% (lowercase, no leading /)", got)
		}
	}
}

func TestBuildWhere_NameEmptyRejected(t *testing.T) {
	expr := FilterExpr{Key: "name", Val: ""}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, want 1=0", where)
	}
}

func TestBuildWhere_Size(t *testing.T) {
	cases := []struct {
		in     string
		wantOp string
		wantN  int64
	}{
		{">=10MB", ">=", 10 * 1024 * 1024},
		{"<2gb", "<", 2 * 1024 * 1024 * 1024},
		{"512", "=", 512},
		{">100kb", ">", 100 * 1024},
	}
	for _, tc := range cases {
		expr := FilterExpr{Key: "size", Val: tc.in}
		where, args, _ := buildWhere(expr)
		if !strings.Contains(where, "i.file_size") {
			t.Errorf("size %q: where = %q", tc.in, where)
		}
		if !strings.Contains(where, tc.wantOp) {
			t.Errorf("size %q: op missing in %q", tc.in, where)
		}
		if len(args) != 1 || args[0] != tc.wantN {
			t.Errorf("size %q: args = %v, want [%d]", tc.in, args, tc.wantN)
		}
	}
}

func TestBuildWhere_SizeUnitless(t *testing.T) {
	// Bare numbers are bytes.
	expr := FilterExpr{Key: "size", Val: ">=1024"}
	_, args, _ := buildWhere(expr)
	if args[0] != int64(1024) {
		t.Errorf("args[0] = %v, want 1024 bytes", args[0])
	}
}

func TestBuildWhere_SizeBadSuffixRejected(t *testing.T) {
	expr := FilterExpr{Key: "size", Val: ">=10ZB"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, want 1=0 for invalid suffix", where)
	}
}

func TestBuildWhere_Mime(t *testing.T) {
	cases := []struct {
		in    string
		wants []string
	}{
		{"png", []string{"'png'"}},
		{"image/jpeg", []string{"'jpeg'"}},
		{"png,jpeg", []string{"'jpeg'", "'png'"}},
		{"bogus", []string{"1=0"}},
	}
	for _, tc := range cases {
		expr := FilterExpr{Key: "mime", Val: tc.in}
		where, _, _ := buildWhere(expr)
		for _, want := range tc.wants {
			if !strings.Contains(where, want) {
				t.Errorf("mime %q: where missing %q (got %q)", tc.in, want, where)
			}
		}
	}
}

func TestBuildWhere_Ratio(t *testing.T) {
	expr := FilterExpr{Key: "ratio", Val: ">=1.5"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "CAST(i.width AS REAL)") {
		t.Errorf("where = %q, want a width/height float ratio", where)
	}
	if len(args) != 1 || args[0] != 1.5 {
		t.Errorf("args = %v, want [1.5]", args)
	}
}

func TestBuildWhere_RatioBadValueRejected(t *testing.T) {
	expr := FilterExpr{Key: "ratio", Val: ">=abc"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, want 1=0", where)
	}
}

func TestBuildWhere_TagCount(t *testing.T) {
	expr := FilterExpr{Key: "tagcount", Val: ">=5"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "FROM image_tags") {
		t.Errorf("where = %q, want a tag-count subquery", where)
	}
	if len(args) != 1 || args[0] != int64(5) {
		t.Errorf("args = %v, want [5]", args)
	}
}

func TestBuildWhere_Duration(t *testing.T) {
	// Comparison rides duration_seconds with an IS NOT NULL guard so
	// non-videos drop out.
	expr := FilterExpr{Key: "duration", Val: ">=30"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "i.duration_seconds IS NOT NULL") {
		t.Errorf("where = %q, want NULL guard", where)
	}
	if !strings.Contains(where, "i.duration_seconds >=") {
		t.Errorf("where = %q, want the comparison", where)
	}
	if len(args) != 1 || args[0] != 30.0 {
		t.Errorf("args = %v, want [30.0]", args)
	}
}

func TestBuildWhere_Hash(t *testing.T) {
	expr := FilterExpr{Key: "hash", Val: "ABCDEF01"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "i.sha256 = ?") {
		t.Errorf("where = %q", where)
	}
	if len(args) != 1 || args[0] != "abcdef01" {
		t.Errorf("args = %v, want the lowercase digest", args)
	}
}

func TestBuildWhere_Prompt(t *testing.T) {
	expr := FilterExpr{Key: "prompt", Val: "1girl"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "sd_metadata sm") || !strings.Contains(where, "comfyui_metadata cm") {
		t.Errorf("where = %q, want both metadata tables", where)
	}
	if len(args) != 2 {
		t.Errorf("args len = %d, want 2 (one per table)", len(args))
	}
}

func TestBuildWhere_Seed(t *testing.T) {
	expr := FilterExpr{Key: "seed", Val: "12345"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "FROM sd_metadata WHERE seed = ?") {
		t.Errorf("where = %q, want IN-subquery shape that pins idx_sd_metadata_seed", where)
	}
	if !strings.Contains(where, "FROM comfyui_metadata WHERE seed = ?") {
		t.Errorf("where = %q, want comfyui IN-subquery", where)
	}
	if len(args) != 2 || args[0] != int64(12345) {
		t.Errorf("args = %v, want both seed slots set to 12345", args)
	}
}

func TestBuildWhere_SeedBadValueRejected(t *testing.T) {
	expr := FilterExpr{Key: "seed", Val: "notanumber"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, want 1=0", where)
	}
}

func TestBuildWhere_Via(t *testing.T) {
	expr := FilterExpr{Key: "via", Val: "upload"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "i.origin = ?") {
		t.Errorf("where = %q", where)
	}
	if len(args) != 1 || args[0] != "upload" {
		t.Errorf("args = %v, want [upload]", args)
	}
}

func TestBuildWhere_UnknownFilter(t *testing.T) {
	// Unknown keys with a non-empty value are treated as category-qualified
	// tag searches. The nil-db builder treats every prefix as a real
	// category, so the bare-prefix form routes through the cat:<key>
	// EXISTS clause instead of collapsing to match-all.
	expr := FilterExpr{Key: "bogus", Val: "val"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "EXISTS") {
		t.Errorf("unknown key:val should yield category-qualified EXISTS clause, got %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args (tag name + category name), got %d: %v", len(args), args)
	}

	// Unknown key with empty value: the nil-db builder treats every
	// prefix as a real category, so the bare form routes to the
	// cat:<key> EXISTS clause (1 arg). The DB-aware builder uses
	// categoryExists to refuse the rewrite when the prefix isn't a
	// real category, falling back to literal tag matching.
	expr2 := FilterExpr{Key: "bogus", Val: ""}
	where2, args2, _ := buildWhere(expr2)
	if !strings.Contains(where2, "tc.name = ?") {
		t.Errorf("bare-prefix should route to category EXISTS, got %q", where2)
	}
	if len(args2) != 1 || args2[0] != "bogus" {
		t.Errorf("expected single-arg category-name, got %v", args2)
	}
}

func TestBuildWhereDB_ColonFallsBackToLiteral(t *testing.T) {
	// When the prefix before `:` is not a real category, the DB-aware
	// builder must match the whole token as a literal tag name so
	// colon-bearing tags like "nier:automata" stay searchable by typing
	// them verbatim.
	database, _ := setupSearchDB(t)

	expr := FilterExpr{Key: "nier", Val: "automata"}
	where, args, _ := buildWhereDB(expr, database)
	if strings.Contains(where, "tc.name") {
		t.Errorf("literal-tag branch should not reference tc.name, got: %q", where)
	}
	if len(args) != 1 || args[0] != "nier:automata" {
		t.Errorf("args = %v, want [nier:automata]", args)
	}
}

func TestBuildWhereDB_ColonUsesCategoryWhenPrefixExists(t *testing.T) {
	// When the prefix IS a real category name, the DB-aware builder
	// must take the category-qualified branch. The pre-resolve helper
	// looks up the canonical tag IDs for `artist:foo` and inlines them
	// as an EXISTS over image_tags; for an unknown tag the resolved
	// set is empty and the predicate is the no-match short-circuit.
	database, _ := setupSearchDB(t)
	var artistID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'artist'`).Scan(&artistID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id) VALUES ('foo', ?)`, artistID,
	); err != nil {
		t.Fatal(err)
	}
	expr := FilterExpr{Key: "artist", Val: "foo"}
	where, _, _ := buildWhereDB(expr, database)
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM image_tags it") {
		t.Errorf("category-qualified branch should emit an inlined image_tags EXISTS, got: %q", where)
	}
	if !strings.Contains(where, "it.tag_id IN (") {
		t.Errorf("category-qualified branch should inline the tag id list, got: %q", where)
	}
}

func TestBuildDateFilter_After(t *testing.T) {
	// `>YYYY-MM-DD` means strictly after day X, so the bound becomes
	// the end-of-day timestamp; without the extension a row ingested
	// at e.g. 2024-01-01T10:00:00Z would still satisfy `> 2024-01-01`
	// (lexicographic compare), which contradicts the intent.
	b := &whereBuilder{}
	clause := b.buildDateFilter(">2024-01-01")
	if !strings.Contains(clause, "> ?") || b.args[0] != "2024-01-01T23:59:59Z" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_Before(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("<2024-12-31")
	if !strings.Contains(clause, "< ?") || b.args[0] != "2024-12-31" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_AfterOrEqual(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter(">=2024-01-01")
	if !strings.Contains(clause, ">= ?") || b.args[0] != "2024-01-01" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_BeforeOrEqual(t *testing.T) {
	// `<=YYYY-MM-DD` extends the payload to T23:59:59Z so a row whose
	// ISO timestamp lives later in the day still matches the bound.
	// Without the end-of-day extension the bare-day form sorts before
	// every real timestamp and excludes every row from day X.
	b := &whereBuilder{}
	clause := b.buildDateFilter("<=2024-12-31")
	if !strings.Contains(clause, "<= ?") || b.args[0] != "2024-12-31T23:59:59Z" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_Range(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-01-01..2024-12-31")
	if !strings.Contains(clause, "BETWEEN") {
		t.Errorf("range clause = %q", clause)
	}
	if len(b.args) != 2 {
		t.Errorf("expected 2 args, got %d", len(b.args))
	}
}

func TestBuildDateFilter_Exact(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-06-15")
	if !strings.Contains(clause, "BETWEEN") {
		t.Errorf("exact date clause = %q", clause)
	}
}

func TestParseCompOp_GTE(t *testing.T) {
	op, val := parseCompOp(">=1920")
	if op != ">=" || val != "1920" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_LTE(t *testing.T) {
	op, val := parseCompOp("<=768")
	if op != "<=" || val != "768" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_GT(t *testing.T) {
	op, val := parseCompOp(">100")
	if op != ">" || val != "100" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_LT(t *testing.T) {
	op, val := parseCompOp("<200")
	if op != "<" || val != "200" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_EQ(t *testing.T) {
	op, val := parseCompOp("=1024")
	if op != "=" || val != "1024" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_Default(t *testing.T) {
	op, val := parseCompOp("512")
	if op != "=" || val != "512" {
		t.Errorf("default op=%q val=%q", op, val)
	}
}

func TestBuildWhere_OR(t *testing.T) {
	expr := OrExpr{
		Left:  TagExpr{Tag: "cat"},
		Right: TagExpr{Tag: "dog"},
	}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "OR") {
		t.Errorf("OR clause = %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}

func TestBuildWhere_NOT(t *testing.T) {
	expr := NotExpr{Expr: TagExpr{Tag: "ugly"}}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT") {
		t.Errorf("NOT clause = %q", where)
	}
}

func TestBuildWhere_AND(t *testing.T) {
	expr := AndExpr{
		Left:  TagExpr{Tag: "cat"},
		Right: TagExpr{Tag: "cute"},
	}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "AND") {
		t.Errorf("AND clause = %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}

func TestBuildWhere_AND_LeftUnknown(t *testing.T) {
	// Bare-prefix filter on the nil-db builder routes to a cat:<key>
	// EXISTS clause (1 arg for the category name); the AND adds one
	// more arg for the right-hand tag search.
	expr := AndExpr{
		Left:  FilterExpr{Key: "bogus", Val: ""},
		Right: TagExpr{Tag: "cute"},
	}
	_, args, _ := buildWhere(expr)
	if len(args) != 2 {
		t.Errorf("expected 2 args (category + tag), got %d: %v", len(args), args)
	}
}

func TestSidebarTagsWithGlobalCount_Empty(t *testing.T) {
	database, _ := setupSearchDB(t)
	tags, err := SidebarTagsWithGlobalCount(database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags != nil {
		t.Error("expected nil for empty image IDs")
	}
}

func TestSidebarTagsWithGlobalCount_WithImages(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	// Both images get a shared tag so the sidebar aggregator has work.
	var catID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	res, err := database.Write.Exec(`INSERT INTO tags (name, category_id, usage_count) VALUES ('shared', ?, 2)`, catID)
	if err != nil {
		t.Fatal(err)
	}
	tagID, _ = res.LastInsertId()

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 images, got %d", len(result.Results))
	}
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img.ID, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SidebarTagsWithGlobalCount(database, ids)
	if err != nil {
		t.Fatal(err)
	}
	var found *int
	for i, tag := range got {
		if tag.Name == "shared" {
			i := i
			found = &i
			break
		}
	}
	if found == nil {
		t.Fatalf("expected tag 'shared' in sidebar aggregator, got %+v", got)
	}
	if got[*found].UsageCount != 2 {
		t.Errorf("shared tag usage = %d, want 2", got[*found].UsageCount)
	}
}

func TestExecute_SortFilesize(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "s1.png")
	ingestTestImage(t, database, cfg, "s2.png")

	result, err := Execute(database, Query{Sort: "filesize", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

func TestExecute_SortRandom(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "r1.png")

	_, err := Execute(database, Query{Sort: "random", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecute_SortOrder_GroupsBySeriesThenOrder(t *testing.T) {
	database, cfg := setupSearchDB(t)
	// Three rows in series "A" with explicit + NULL order, one row in
	// series "B", one row with no series at all. Verify that the result
	// groups by series alphabetically (empty first), then by
	// series_order with NULL last.
	ingestTestImage(t, database, cfg, "a1.png")
	ingestTestImage(t, database, cfg, "a2.png")
	ingestTestImage(t, database, cfg, "a3.png")
	ingestTestImage(t, database, cfg, "b1.png")
	ingestTestImage(t, database, cfg, "none.png")
	imageID := func(name string) int64 {
		var id int64
		if err := database.Read.QueryRow(
			`SELECT id FROM images WHERE canonical_path LIKE '%' || ? ORDER BY id LIMIT 1`, name,
		).Scan(&id); err != nil {
			t.Fatalf("look up %q: %v", name, err)
		}
		return id
	}
	a1, a2, a3, b1, none := imageID("a1.png"), imageID("a2.png"), imageID("a3.png"), imageID("b1.png"), imageID("none.png")
	for _, set := range []struct {
		id    int64
		ser   string
		order interface{}
	}{
		{a1, "A", 2},
		{a2, "A", 1},
		{a3, "A", nil},
		{b1, "B", 1},
	} {
		if _, err := database.Write.Exec(`UPDATE images SET series=?, series_order=? WHERE id=?`, set.ser, set.order, set.id); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Execute(database, Query{Sort: "order", Order: "asc", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(result.Results))
	}
	gotOrder := make([]int64, len(result.Results))
	for i, r := range result.Results {
		gotOrder[i] = r.ID
	}
	want := []int64{none, a2, a1, a3, b1}
	for i, id := range want {
		if gotOrder[i] != id {
			t.Errorf("position %d: id=%d, want %d (full order: %v)", i, gotOrder[i], id, gotOrder)
		}
	}
}

func TestExecute_FavFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "fav1.png")

	expr := FilterExpr{Key: "fav", Val: "true"}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 favorited images, got %d", result.Total)
	}
}

// New ingests land in the inbox (is_inbox=1 by column default), so the
// inbox:true filter returns the freshly-ingested row and the IsInbox
// projection round-trips through the executor's main SELECT/Scan.
func TestExecute_InboxFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "inbox1.png")

	resTrue, err := Execute(database, Query{Expr: FilterExpr{Key: "inbox", Val: "true"}, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if resTrue.Total != 1 {
		t.Errorf("inbox:true Total = %d, want 1", resTrue.Total)
	}
	if len(resTrue.Results) != 1 || !resTrue.Results[0].IsInbox {
		t.Errorf("inbox:true result didn't surface IsInbox=true: %+v", resTrue.Results)
	}

	resFalse, err := Execute(database, Query{Expr: FilterExpr{Key: "inbox", Val: "false"}, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if resFalse.Total != 0 {
		t.Errorf("inbox:false Total = %d, want 0", resFalse.Total)
	}
}

func TestExecute_TagSearch(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "tagged.png")

	// Even without tags, tag search should return correct results
	expr := TagExpr{Tag: "nonexistent_tag_xyz"}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 results for nonexistent tag, got %d", result.Total)
	}
}

func TestExecute_SkipCount(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "sc1.png")
	ingestTestImage(t, database, cfg, "sc2.png")

	result, err := Execute(database, Query{Page: 1, Limit: 40, SkipCount: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0 (skip-count)", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("Results = %d, want 2", len(result.Results))
	}
}

func TestExecute_DefaultPagination(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "p1.png")

	// page=0 and limit=0 should use defaults
	result, err := Execute(database, Query{Page: 0, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if result.Page != 1 {
		t.Errorf("default page = %d, want 1", result.Page)
	}
	if result.Limit != 40 {
		t.Errorf("default limit = %d, want 40", result.Limit)
	}
}

func TestExecuteAdjacent_Newest(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "adj_a.png")
	ingestTestImage(t, database, cfg, "adj_b.png")
	ingestTestImage(t, database, cfg, "adj_c.png")

	result, err := Execute(database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	// result.Results is sorted newest→oldest: [newest, middle, oldest].
	newest, middle, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	// Middle image: prev is the newer (newest), next is the older (oldest).
	prev, next, err := ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, middle)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != newest {
		t.Errorf("middle prev = %v, want %d", prev, newest)
	}
	if next == nil || *next != oldest {
		t.Errorf("middle next = %v, want %d", next, oldest)
	}

	// Edge: newest has no prev, still has next.
	prev, next, _ = ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, newest)
	if prev != nil {
		t.Errorf("newest prev = %v, want nil", prev)
	}
	if next == nil || *next != middle {
		t.Errorf("newest next = %v, want %d", next, middle)
	}

	// Edge: oldest has no next, still has prev.
	prev, next, _ = ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, oldest)
	if next != nil {
		t.Errorf("oldest next = %v, want nil", next)
	}
	if prev == nil || *prev != middle {
		t.Errorf("oldest prev = %v, want %d", prev, middle)
	}
}

func TestExecuteAdjacent_Random(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_a.png")
	ingestTestImage(t, database, cfg, "rnd_b.png")
	ingestTestImage(t, database, cfg, "rnd_c.png")

	const seed int64 = 1234567
	q := Query{Sort: "random", RandomSeed: seed, Page: 1, Limit: 40}
	result, err := Execute(database, q)
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	first, second, third := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	prev, next, err := ExecuteAdjacent(database, q, second)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != first {
		t.Errorf("middle prev = %v, want %d", prev, first)
	}
	if next == nil || *next != third {
		t.Errorf("middle next = %v, want %d", next, third)
	}

	prev, next, _ = ExecuteAdjacent(database, q, first)
	if prev != nil {
		t.Errorf("first prev = %v, want nil", prev)
	}
	if next == nil || *next != second {
		t.Errorf("first next = %v, want %d", next, second)
	}

	prev, next, _ = ExecuteAdjacent(database, q, third)
	if next != nil {
		t.Errorf("third next = %v, want nil", next)
	}
	if prev == nil || *prev != second {
		t.Errorf("third prev = %v, want %d", prev, second)
	}
}

func TestExecuteAdjacent_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_no_seed.png")
	prev, next, err := ExecuteAdjacent(database, Query{Sort: "random"}, 1)
	if err != nil || prev != nil || next != nil {
		t.Errorf("random adjacency without seed must be nil/nil, got prev=%v next=%v err=%v", prev, next, err)
	}
}

func TestRankInQuery_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_rnd.png")
	q := Query{Sort: "random", RandomSeed: 0, Page: 1, Limit: 40}
	rank, err := RankInQuery(context.Background(), database, q, 1)
	if err != nil || rank != -1 {
		t.Errorf("random sort with seed=0 must be (-1, nil), got (%d, %v)", rank, err)
	}
}

// A small caller-supplied seed (e.g. `seed=1` on the API) must
// produce a shuffled order, not strict id-ASC. The bare
// `(id * seed) & 2^31-1` formula collapses to identity for small
// seeds; mixSeed keeps the multiplier above 2^30 so the masking
// actually permutes.
func TestExecute_RandomSmallSeedShuffles(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 8; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("rnd_small_%d.png", i))
	}

	q := Query{Sort: "random", RandomSeed: 1, Page: 1, Limit: 40}
	res, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) < 8 {
		t.Fatalf("got %d rows, want 8", len(res.Results))
	}
	asc := true
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i].ID <= res.Results[i-1].ID {
			asc = false
			break
		}
	}
	if asc {
		t.Errorf("seed=1 returned strict id-ASC; expected a shuffle: %+v", res.Results)
	}
}

// The mix step is deterministic: the same seed produces the same
// order on every Execute call.
func TestExecute_RandomSeedStableAcrossCalls(t *testing.T) {
	database, env := setupSearchDB(t)
	for i := 0; i < 6; i++ {
		ingestTestImage(t, database, env, fmt.Sprintf("rnd_stable_%d.png", i))
	}

	q := Query{Sort: "random", RandomSeed: 42, Page: 1, Limit: 40}
	first, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != len(second.Results) {
		t.Fatalf("call returned %d vs %d rows", len(first.Results), len(second.Results))
	}
	for i := range first.Results {
		if first.Results[i].ID != second.Results[i].ID {
			t.Errorf("position %d: first %d, second %d (order not stable)", i, first.Results[i].ID, second.Results[i].ID)
		}
	}
}

func TestRankInQuery_RandomSeeded(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_a.png")
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_b.png")
	ingestTestImage(t, database, cfg, "rank_rnd_seeded_c.png")
	q := Query{Sort: "random", RandomSeed: 1234567, Page: 1, Limit: 40}
	res, err := Execute(database, q)
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(res.Results))
	}
	for i, img := range res.Results {
		rank, err := RankInQuery(context.Background(), database, q, img.ID)
		if err != nil {
			t.Fatalf("rank for id=%d: %v", img.ID, err)
		}
		if rank != i {
			t.Errorf("id=%d: rank = %d, want %d (matches Execute order)", img.ID, rank, i)
		}
	}
}

func TestRankInQuery_HonorsContextDeadline(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rank_ctx.png")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rank, err := RankInQuery(ctx, database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40}, 1)
	if err == nil || rank != -1 {
		t.Errorf("cancelled context must surface err and rank=-1, got (%d, %v)", rank, err)
	}
}

func TestExecuteAdjacent_WithTagPredicate(t *testing.T) {
	// Tuple cursor with a tag-predicate WHERE: LIMIT 1 must walk past
	// untagged neighbours and land on the next tagged one.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "adj_tag_a.png")
	ingestTestImage(t, database, env, "adj_tag_b.png")
	ingestTestImage(t, database, env, "adj_tag_c.png")

	result, err := Execute(database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	newest, _, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blue', ?, 2) RETURNING id`,
		generalID,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{newest, oldest} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{Expr: TagExpr{Tag: "blue"}, Sort: "newest", Order: "desc"}

	prev, next, err := ExecuteAdjacent(database, q, newest)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("newest prev = %v, want nil", prev)
	}
	if next == nil || *next != oldest {
		t.Errorf("newest next = %v, want %d", next, oldest)
	}

	prev, next, _ = ExecuteAdjacent(database, q, oldest)
	if prev == nil || *prev != newest {
		t.Errorf("oldest prev = %v, want %d", prev, newest)
	}
	if next != nil {
		t.Errorf("oldest next = %v, want nil", next)
	}
}

func TestExecuteAdjacent_RandomBucketBound(t *testing.T) {
	// Random sort + tag predicate bounds adjacency to a fixed id-range
	// bucket when the candidate set is dense enough to need it. Images
	// outside the current image's bucket must not appear as prev/next,
	// even if they match the predicate. usage_count is set above
	// fastApproxThreshold so adjacencyTotalEstimate keeps the gate on -
	// the sparse-skip path is covered separately.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rb_a.png")
	ingestTestImage(t, database, env, "rb_b.png")
	ingestTestImage(t, database, env, "rb_c.png")

	var nearA, nearB, far int64
	rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	nearA, nearB = ids[0], ids[1]
	far = int64(randomAdjacencyBucketSize) + 1
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE image_paths SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('rndtag', ?, ?) RETURNING id`,
		generalID, fastApproxThreshold,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{nearA, nearB, far} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{Expr: TagExpr{Tag: "rndtag"}, Sort: "random", RandomSeed: 1234567}

	// nearA's bucket holds nearA + nearB; far is in a different bucket.
	prev, next, err := ExecuteAdjacent(database, q, nearA)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*int64{prev, next} {
		if p != nil && *p == far {
			t.Errorf("nearA reached far image %d across bucket boundary", far)
		}
	}
	reachedNearB := (prev != nil && *prev == nearB) || (next != nil && *next == nearB)
	if !reachedNearB {
		t.Errorf("nearA did not reach in-bucket peer %d (prev=%v next=%v)", nearB, prev, next)
	}

	// far is alone in its bucket.
	prev, next, _ = ExecuteAdjacent(database, q, far)
	if prev != nil || next != nil {
		t.Errorf("far alone in bucket: want nil/nil, got prev=%v next=%v", prev, next)
	}
}

// TestExecuteAdjacent_NewestSparseAndBucketBound pins the bucket-gate: a
// 3-AND back_q under newest sort gates prev/next to an id-window when
// the candidate set is dense enough to need it, so a sparse intersection
// late in the result set can't force a multi-second scan. The 2-AND
// shape keeps the pre-existing unbounded behaviour because the cursor
// walk is acceptable there. Each leaf carries usage_count above
// fastApproxThreshold so adjacencyTotalEstimate keeps the gate on; the
// candidate-too-small skip path is covered in its own test.
func TestExecuteAdjacent_NewestSparseAndBucketBound(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ad_a.png")
	ingestTestImage(t, database, env, "ad_b.png")
	ingestTestImage(t, database, env, "ad_c.png")

	rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	nearA, nearB := ids[0], ids[1]
	// Push the third image into a different id-bucket. With
	// andAdjacencyBucketSize = 10000, ids 1-10000 share a bucket; jump
	// past the next bucket boundary so far is unambiguously outside.
	far := int64(andAdjacencyBucketSize) * 2
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE image_paths SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	mkTag := func(name string) int64 {
		var id int64
		if err := database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, ?) RETURNING id`,
			name, generalID, fastApproxThreshold,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := mkTag("ada")
	b := mkTag("adb")
	c := mkTag("adc")
	for _, p := range []struct {
		img int64
		tag int64
	}{
		{nearA, a}, {nearA, b}, {nearA, c},
		{nearB, a}, {nearB, b}, {nearB, c},
		{far, a}, {far, b}, {far, c},
	} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, p.img, p.tag,
		); err != nil {
			t.Fatal(err)
		}
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "ada"},
			Right: TagExpr{Tag: "adb"},
		},
		Right: TagExpr{Tag: "adc"},
	}
	q := Query{Expr: expr, Sort: "newest", Order: "desc"}

	// nearA's bucket holds nearA + nearB; far sits in a later bucket.
	prev, next, err := ExecuteAdjacent(database, q, nearA)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*int64{prev, next} {
		if p != nil && *p == far {
			t.Errorf("nearA reached far image %d across bucket boundary", far)
		}
	}
	reachedNearB := (prev != nil && *prev == nearB) || (next != nil && *next == nearB)
	if !reachedNearB {
		t.Errorf("nearA did not reach in-bucket peer %d (prev=%v next=%v)", nearB, prev, next)
	}

	// far is alone in its own bucket; bucket gate stops prev/next there.
	prev, next, _ = ExecuteAdjacent(database, q, far)
	if prev != nil || next != nil {
		t.Errorf("far alone in bucket: want nil/nil, got prev=%v next=%v", prev, next)
	}

	// Sanity: 2-AND back_q does NOT engage the gate (only 3+ ANDs
	// trigger it), so far is reachable in the same fixture under a
	// 2-AND query.
	expr2 := AndExpr{Left: TagExpr{Tag: "ada"}, Right: TagExpr{Tag: "adb"}}
	q2 := Query{Expr: expr2, Sort: "newest", Order: "desc"}
	prev, next, _ = ExecuteAdjacent(database, q2, nearA)
	if prev == nil && next == nil {
		t.Errorf("2-AND adjacency on nearA returned nothing; expected to reach a peer")
	}
}

// TestExecuteAdjacent_RandomSparseSkipsBucket pins the inverse of
// RandomBucketBound: a tag-predicate query whose candidate-set upper
// bound sits below fastApproxThreshold skips the id-bucket gate so
// prev/next reaches every match instead of dying at a bucket edge that
// holds only currentID.
func TestExecuteAdjacent_RandomSparseSkipsBucket(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rs_a.png")
	ingestTestImage(t, database, env, "rs_b.png")
	ingestTestImage(t, database, env, "rs_c.png")

	rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	nearA, nearB := ids[0], ids[1]
	far := int64(randomAdjacencyBucketSize) * 2
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE image_paths SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	// usage_count = 3 keeps the candidate upper bound below
	// fastApproxThreshold; the bucket gate must skip.
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('rstag', ?, 3) RETURNING id`,
		generalID,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{nearA, nearB, far} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{Expr: TagExpr{Tag: "rstag"}, Sort: "random", RandomSeed: 1234567}

	// All 3 matches must be reachable via the prev+next chain - the
	// bucket gate would have stopped at nearA's id-window.
	reached := walkAdjacency(t, database, q, nearA)
	for _, want := range []int64{nearA, nearB, far} {
		if !reached[want] {
			t.Errorf("sparse random adjacency did not reach %d; reached=%v", want, reached)
		}
	}
}

// TestExecuteAdjacent_NewestSparseAndSkipsBucket mirrors the random-sort
// skip for the newest-sort 3+AND gate: a candidate set below
// fastApproxThreshold rides the AND-driver's single-leg path unbounded
// instead of capping at the andAdjacencyBucketSize boundary.
func TestExecuteAdjacent_NewestSparseAndSkipsBucket(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ns_a.png")
	ingestTestImage(t, database, env, "ns_b.png")
	ingestTestImage(t, database, env, "ns_c.png")

	rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	nearA, nearB := ids[0], ids[1]
	far := int64(andAdjacencyBucketSize) * 2
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE image_paths SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	mkTag := func(name string) int64 {
		var id int64
		if err := database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 3) RETURNING id`,
			name, generalID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := mkTag("nsa")
	b := mkTag("nsb")
	c := mkTag("nsc")
	for _, p := range []struct {
		img int64
		tag int64
	}{
		{nearA, a}, {nearA, b}, {nearA, c},
		{nearB, a}, {nearB, b}, {nearB, c},
		{far, a}, {far, b}, {far, c},
	} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, p.img, p.tag,
		); err != nil {
			t.Fatal(err)
		}
	}

	expr := AndExpr{
		Left: AndExpr{
			Left:  TagExpr{Tag: "nsa"},
			Right: TagExpr{Tag: "nsb"},
		},
		Right: TagExpr{Tag: "nsc"},
	}
	q := Query{Expr: expr, Sort: "newest", Order: "desc"}

	// far must be reachable from nearA's id-bucket: bucket gate is
	// skipped because the 3-AND's smallest leaf has usage_count well
	// below fastApproxThreshold.
	reached := walkAdjacency(t, database, q, nearA)
	for _, want := range []int64{nearA, nearB, far} {
		if !reached[want] {
			t.Errorf("sparse 3-AND adjacency did not reach %d; reached=%v", want, reached)
		}
	}
}

// walkAdjacency returns the set of image ids reachable from start by
// following ExecuteAdjacent's prev/next chain transitively. Used by
// the sparse-skip tests to assert that every match in the candidate
// set is reachable instead of stopping at a bucket edge.
func walkAdjacency(t *testing.T, database *db.DB, q Query, start int64) map[int64]bool {
	t.Helper()
	reached := map[int64]bool{start: true}
	queue := []int64{start}
	for len(queue) > 0 {
		cursor := queue[0]
		queue = queue[1:]
		prev, next, err := ExecuteAdjacent(database, q, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, neighbour := range []*int64{prev, next} {
			if neighbour == nil || reached[*neighbour] {
				continue
			}
			reached[*neighbour] = true
			queue = append(queue, *neighbour)
		}
	}
	return reached
}

func TestExecuteAdjacent_RatingCeiling(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "adj_safe.png")
	ingestTestImage(t, database, env, "adj_explicit.png")
	ingestTestImage(t, database, env, "adj_safe2.png")

	var safeID, expID, safe2ID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%adj_safe.png'`).Scan(&safeID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%adj_explicit.png'`).Scan(&expID)
	database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path LIKE '%adj_safe2.png'`).Scan(&safe2ID)

	attachTag(t, database, safeID, ratingTagID(t, database, "general"))
	attachTag(t, database, expID, ratingTagID(t, database, "explicit"))
	attachTag(t, database, safe2ID, ratingTagID(t, database, "general"))

	// Ceiling = sensitive: NOT rating:questionable AND NOT rating:explicit.
	expr := AndExpr{
		Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
		Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
	}
	q := Query{Expr: expr, Sort: "newest", Order: "desc"}

	// Display order is [safe2 (newest), explicit, safe (oldest)]. In desc
	// sort `prev` is the newer neighbour, so safe.prev without the ceiling
	// is the explicit middle row; with the ceiling it must skip the hidden
	// row and land on safe2.
	prev, next, err := ExecuteAdjacent(database, q, safeID)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil {
		t.Fatal("ceiling adjacency: prev = nil, want safe2ID")
	}
	if *prev == expID {
		t.Errorf("ceiling adjacency leaked the explicit image: prev = %d", *prev)
	}
	if *prev != safe2ID {
		t.Errorf("ceiling adjacency: prev = %d, want %d", *prev, safe2ID)
	}
	if next != nil {
		t.Errorf("oldest next under ceiling: got %v, want nil", next)
	}
}

// TestExecuteAdjacent_FolderHalfOpenRange pins the folder-adjacency
// path: a pure folder back_q rides a half-open path range so the cursor seeks
// idx_images_folder_visible. The match set must equal the slow path's
// (folder = ? OR folder LIKE 'X/%' ESCAPE '\') form: the folder
// itself plus everything beneath it, and nothing outside the prefix.
func TestExecuteAdjacent_FolderHalfOpenRange(t *testing.T) {
	database, env := setupSearchDB(t)
	mkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(env.galleryDir, rel), 0755); err != nil {
			t.Fatal(err)
		}
	}
	ingestAt := func(rel, name string) int64 {
		t.Helper()
		ingestCounter++
		dir := filepath.Join(env.galleryDir, rel)
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 10+ingestCounter, 10))); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
		if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png", ""); err != nil {
			t.Fatalf("ingest %q: %v", path, err)
		}
		var id int64
		if err := database.Read.QueryRow(
			`SELECT id FROM images WHERE canonical_path = ?`, path,
		).Scan(&id); err != nil {
			t.Fatalf("lookup %q: %v", path, err)
		}
		return id
	}

	mkdir("anime")
	mkdir("anime/girls")
	mkdir("animes")
	mkdir("anime-2024")
	rootImg := ingestAt(".", "fol_root.png")
	folderImg := ingestAt("anime", "fol_a.png")
	// "anime-2024" sorts between "anime" and "anime0" lexicographically
	// because '-' (0x2D) is below '/' (0x2F); a naive [val, val+"0")
	// range would leak it. Ingest before subImg so the leaked sibling
	// becomes folderImg's immediate-newer neighbour under desc-newest -
	// otherwise subImg sits between them and the leak hides behind it.
	siblingDashImg := ingestAt("anime-2024", "fol_d.png")
	subImg := ingestAt("anime/girls", "fol_b.png")
	siblingImg := ingestAt("animes", "fol_c.png")

	q := Query{
		Expr:  FilterExpr{Key: "folder", Val: "anime"},
		Sort:  "newest",
		Order: "desc",
	}

	// folderImg's neighbours under folder:anime are subImg (also under
	// anime/) and nothing else - rootImg sits at the gallery root and
	// the two sibling-prefix folders ("animes", "anime-2024") sit
	// outside anime/.
	prev, next, err := ExecuteAdjacent(database, q, folderImg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*int64{prev, next} {
		if p == nil {
			continue
		}
		if *p == rootImg {
			t.Errorf("folder adjacency leaked root image %d", rootImg)
		}
		if *p == siblingImg {
			t.Errorf("folder adjacency leaked sibling-prefix image %d (animes/ outside anime/)", siblingImg)
		}
		if *p == siblingDashImg {
			t.Errorf("folder adjacency leaked low-ASCII sibling image %d (anime-2024/ outside anime/)", siblingDashImg)
		}
	}
	reachedSub := (prev != nil && *prev == subImg) || (next != nil && *next == subImg)
	if !reachedSub {
		t.Errorf("folder adjacency did not reach subfolder peer %d (prev=%v next=%v)", subImg, prev, next)
	}
}

func TestBuildWhere_DateFilter(t *testing.T) {
	where, args, _ := buildWhere(FilterExpr{Key: "date", Val: ">2024-01-01"})
	if !strings.Contains(where, "ingested_at") {
		t.Errorf("date filter missing ingested_at: %q", where)
	}
	if len(args) == 0 {
		t.Error("expected args for date filter")
	}
}

// Malformed date input must emit `1=0` rather than passing the value
// straight into the SQL comparison; the latter silently returned zero
// rows, indistinguishable from a real "no images on that date". The
// regex accepts YYYY, YYYY-MM, and YYYY-MM-DD per HELP.md examples.
func TestBuildWhere_DateFilter_RejectsBadInput(t *testing.T) {
	for _, in := range []string{"abcd", "abcd..xyz", "2024-01..bogus", "2024/01/01", ">notadate", "<not", "20240101"} {
		where, args, _ := buildWhere(FilterExpr{Key: "date", Val: in})
		if !strings.Contains(where, "1=0") {
			t.Errorf("bad date %q: expected 1=0, got %q", in, where)
		}
		if len(args) != 0 {
			t.Errorf("bad date %q: expected no args, got %v", in, args)
		}
	}
}

func TestBuildWhere_DateFilter_AcceptsYearMonth(t *testing.T) {
	for _, in := range []string{"2024", "2024-06", "2024-06-15", ">2024-01", "<2099-12", "2000-01..2099-12"} {
		where, _, _ := buildWhere(FilterExpr{Key: "date", Val: in})
		if strings.Contains(where, "1=0") {
			t.Errorf("good date %q rejected: %q", in, where)
		}
	}
}

func TestExecute_WithAutoTaggedAt(t *testing.T) {
	// Test that Execute correctly parses auto_tagged_at when set
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "autotagged.png")

	// Set auto_tagged_at directly in DB
	database.Write.Exec(`UPDATE images SET auto_tagged_at = '2024-01-15T12:00:00Z'`)

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == 0 {
		t.Skip("no images in DB")
	}
	// At least one image should have AutoTaggedAt set
	found := false
	for _, img := range result.Results {
		if img.AutoTaggedAt != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one image with AutoTaggedAt set")
	}
}

func TestSuggestTagsWithFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "combo1.png")
	ingestTestImage(t, database, cfg, "combo2.png")
	ingestTestImage(t, database, cfg, "combo3.png")

	// Grab image IDs in insertion order.
	rows, _ := database.Read.Query(`SELECT id FROM images ORDER BY id`)
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}

	var catID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&catID)
	seed := func(name string) int64 {
		res, err := database.Write.Exec(`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, name, catID)
		if err != nil {
			t.Fatalf("seed tag %q: %v", name, err)
		}
		tid, _ := res.LastInsertId()
		return tid
	}
	tagA := seed("alpha")
	tagB := seed("beta")
	tagBet := seed("betula")

	add := func(img, tag int64) {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tag,
		); err != nil {
			t.Fatalf("add tag: %v", err)
		}
		database.Write.Exec(`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tag)
	}
	// img1: alpha, beta   img2: alpha, beta   img3: betula only
	add(ids[0], tagA)
	add(ids[0], tagB)
	add(ids[1], tagA)
	add(ids[1], tagB)
	add(ids[2], tagBet)

	// Typing "be" with no context: both beta (2 images) and betula (1) match.
	got, err := SuggestTagsWithFilter(database, nil, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("plain suggest: expected 2, got %+v", got)
	}
	// Typing "be" with context "alpha": only beta (co-occurs with alpha).
	expr, _ := Parse("alpha")
	got, err = SuggestTagsWithFilter(database, expr, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("context suggest: expected only beta, got %+v", got)
	}
	if got[0].UsageCount != 2 {
		t.Errorf("expected combo count 2 for alpha+beta, got %d", got[0].UsageCount)
	}
}

// TestExecuteAdjacent_CacheReuse pins the gallery -> detail handoff: a
// gallery render whose page-1 result holds the full match set seeds the
// adjacency cache, and ExecuteAdjacent then serves prev/next from the
// cache without touching the cursor SQL.
func TestExecuteAdjacent_CacheReuse(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "cr_a.png")
	ingestTestImage(t, database, env, "cr_b.png")
	ingestTestImage(t, database, env, "cr_c.png")

	q := Query{
		Sort:     "newest",
		Order:    "desc",
		Page:     1,
		Limit:    40,
		CacheKey: "test-cache-reuse",
	}
	res, err := Execute(database, q)
	if err != nil || len(res.Results) != 3 {
		t.Fatalf("Execute: err=%v len=%d", err, len(res.Results))
	}

	cached, ok := AdjacencyCacheGet(q.CacheKey)
	if !ok {
		t.Fatalf("Execute did not populate the cache")
	}
	if len(cached) != 3 {
		t.Fatalf("cache size = %d, want 3", len(cached))
	}
	for i, img := range res.Results {
		if cached[i] != img.ID {
			t.Errorf("cache[%d] = %d, want %d", i, cached[i], img.ID)
		}
	}

	// With a poisoned db handle prev/next must still answer correctly:
	// the cache hit short-circuits before any SQL fires.
	middle := res.Results[1].ID
	prev, next, err := ExecuteAdjacent(nil, q, middle)
	if err != nil {
		t.Fatalf("cached ExecuteAdjacent: %v", err)
	}
	if prev == nil || *prev != res.Results[0].ID {
		t.Errorf("prev = %v, want %d", prev, res.Results[0].ID)
	}
	if next == nil || *next != res.Results[2].ID {
		t.Errorf("next = %v, want %d", next, res.Results[2].ID)
	}
}

// TestExecute_CachesFullMatchListOnMultiPage pins the populate path for
// results that span more than one page: the cache holds the full sorted
// match list (capped at adjacencyCacheMaxIDs), so subsequent gallery
// page-flips and detail prev/next ride the cache instead of re-running
// the search SQL.
func TestExecute_CachesFullMatchListOnMultiPage(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	for i := 0; i < 5; i++ {
		ingestTestImage(t, database, env, "multi_"+string(rune('a'+i))+".png")
	}

	q := Query{
		Sort:     "newest",
		Order:    "desc",
		Page:     1,
		Limit:    2, // page holds 2 of 5; cache must still hold all 5
		CacheKey: "test-cache-multi",
	}
	res, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 || len(res.Results) != 2 {
		t.Fatalf("page 1 result: total=%d len=%d, want 5/2", res.Total, len(res.Results))
	}
	// Multi-page seed runs in the background so page 1 returns at the
	// speed of its data SELECT alone. Poll briefly for the cache write.
	var cached []int64
	var ok bool
	for i := 0; i < 100; i++ {
		if cached, ok = AdjacencyCacheGet(q.CacheKey); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("multi-page render did not populate the cache")
	}
	if len(cached) != 5 {
		t.Fatalf("cached len = %d, want 5", len(cached))
	}
	if cached[0] != res.Results[0].ID || cached[1] != res.Results[1].ID {
		t.Errorf("cache prefix [%d %d] does not match page 1 [%d %d]",
			cached[0], cached[1], res.Results[0].ID, res.Results[1].ID)
	}
}

// TestExecute_CacheServesGalleryReload pins the gallery -> detail -> back
// flow: when the cache holds the match-id list, a second Execute on the
// same key returns the right page rows in the cached order without
// re-running the search SQL. Image rows still scan fresh from the DB so
// row-level fields (favorite, missing) reflect any concurrent mutation.
func TestExecute_CacheServesGalleryReload(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	for i := 0; i < 3; i++ {
		ingestTestImage(t, database, env, "reload_"+string(rune('a'+i))+".png")
	}

	q := Query{
		Sort:     "newest",
		Order:    "desc",
		Page:     1,
		Limit:    40,
		CacheKey: "test-cache-reload",
	}
	first, err := Execute(database, q)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if len(first.Results) != 3 {
		t.Fatalf("first Results = %d, want 3", len(first.Results))
	}
	cached, ok := AdjacencyCacheGet(q.CacheKey)
	if !ok || len(cached) != 3 {
		t.Fatalf("cache state: ok=%v len=%d", ok, len(cached))
	}

	// Flip a row's is_favorited under the cache. The cache holds only
	// ids, so the second Execute must surface the new flag.
	if _, err := database.Write.Exec(
		`UPDATE images SET is_favorited = 1 WHERE id = ?`, first.Results[0].ID,
	); err != nil {
		t.Fatal(err)
	}

	second, err := Execute(database, q)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if second.Total != 3 || len(second.Results) != 3 {
		t.Fatalf("second result: total=%d len=%d, want 3/3", second.Total, len(second.Results))
	}
	for i := range first.Results {
		if first.Results[i].ID != second.Results[i].ID {
			t.Errorf("Results[%d].ID = %d, want %d", i, second.Results[i].ID, first.Results[i].ID)
		}
	}
	if !second.Results[0].IsFavorited {
		t.Errorf("cache hit returned stale is_favorited; row mutation should be visible")
	}
}

// TestExecute_CacheSlicesByPage pins pagination on the cached fast path:
// the populated entry feeds page=2 the right slice and a page past the
// end returns an empty result, all without re-running the search SQL.
func TestExecute_CacheSlicesByPage(t *testing.T) {
	AdjacencyCacheClear()
	database, env := setupSearchDB(t)
	for i := 0; i < 5; i++ {
		ingestTestImage(t, database, env, "slice_"+string(rune('a'+i))+".png")
	}

	cacheKey := "test-cache-slice"
	populate, err := Execute(database, Query{
		Sort: "newest", Order: "desc", Page: 1, Limit: 40, CacheKey: cacheKey,
	})
	if err != nil {
		t.Fatalf("populate Execute: %v", err)
	}
	if len(populate.Results) != 5 {
		t.Fatalf("populate Results = %d, want 5", len(populate.Results))
	}
	cached, ok := AdjacencyCacheGet(cacheKey)
	if !ok || len(cached) != 5 {
		t.Fatalf("cache state: ok=%v len=%d", ok, len(cached))
	}

	res, err := Execute(database, Query{
		Sort: "newest", Order: "desc", Page: 2, Limit: 2, CacheKey: cacheKey,
	})
	if err != nil {
		t.Fatalf("Execute page 2: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("Total = %d, want 5", res.Total)
	}
	if len(res.Results) != 2 {
		t.Fatalf("page 2 Results = %d, want 2", len(res.Results))
	}
	if res.Results[0].ID != cached[2] || res.Results[1].ID != cached[3] {
		t.Errorf("page 2 ids = [%d %d], want [%d %d]",
			res.Results[0].ID, res.Results[1].ID, cached[2], cached[3])
	}

	pastEnd, err := Execute(database, Query{
		Sort: "newest", Order: "desc", Page: 99, Limit: 2, CacheKey: cacheKey,
	})
	if err != nil {
		t.Fatalf("Execute page past end: %v", err)
	}
	if pastEnd.Total != 5 || len(pastEnd.Results) != 0 {
		t.Errorf("past-end result: total=%d len=%d, want 5/0", pastEnd.Total, len(pastEnd.Results))
	}
}

func TestExecute_PhashExact(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")
	// Manually pin one row's phash so we can match it deterministically.
	const want = int64(0x123456789ABCDEF0)
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = 1`, want); err != nil {
		t.Fatal(err)
	}
	q, err := Parse("phash:123456789abcdef0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Execute(database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Results[0].ID != 1 {
		t.Fatalf("phash exact: total=%d ids=%v, want 1/[1]", res.Total, res.Results)
	}
}

func TestExecute_PhashBareIsNotNull(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png") // gets a phash from the thumbnail
	ingestTestImage(t, database, env, "b.png")
	// Manually clear one row's phash.
	if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	q, err := Parse("phash:")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Execute(database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("phash:bare: total=%d, want 1 (one row has NULL phash)", res.Total)
	}
}

func TestExecute_PhashDistanceMatchesViaPopcountFallback(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")
	ingestTestImage(t, database, env, "c.png")
	// Hashes 1 differ from each other by varying Hamming distances.
	// Pin them so the test owns the math.
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = 1`, int64(0xFF00)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = 2`, int64(0xFF01)); err != nil { // 1 bit away from id=1
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = 3`, int64(0x0000)); err != nil { // 8 bits away from id=1
		t.Fatal(err)
	}
	// Query within distance 1 of FF00: expect ids 1 and 2.
	q, err := Parse("phash:000000000000ff00~1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Execute(database, Query{Expr: q, Sort: "newest", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Fatalf("phash:~1 total=%d, want 2", res.Total)
	}
}

func TestExecute_RelationVocabulary(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	c := ingestTestImageWithID(t, database, env, "c.png")
	d := ingestTestImageWithID(t, database, env, "d.png")
	_ = d

	// Manually wire a small relations graph so we don't need the service.
	if _, err := database.Write.Exec(`INSERT INTO dup_groups (id, original_image_id) VALUES (1, ?)`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`INSERT INTO dup_group_members (image_id, group_id) VALUES (?, 1), (?, 1)`, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`INSERT INTO derivative_edges (derivative_image_id, source_image_id) VALUES (?, ?)`, c, a); err != nil {
		t.Fatal(err)
	}

	check := func(query string, wantIDs []int64) {
		t.Helper()
		expr, err := Parse(query)
		if err != nil {
			t.Fatalf("parse %q: %v", query, err)
		}
		res, err := Execute(database, Query{Expr: expr, Sort: "newest", Order: "asc", Page: 1, Limit: 40})
		if err != nil {
			t.Fatalf("execute %q: %v", query, err)
		}
		gotIDs := make([]int64, len(res.Results))
		for i, im := range res.Results {
			gotIDs[i] = im.ID
		}
		if len(gotIDs) != len(wantIDs) {
			t.Fatalf("%q: got ids %v, want %v", query, gotIDs, wantIDs)
		}
		for i := range gotIDs {
			if gotIDs[i] != wantIDs[i] {
				t.Fatalf("%q at %d: got %d, want %d", query, i, gotIDs[i], wantIDs[i])
			}
		}
	}
	check("relation:duplicate", []int64{a, b})
	check("relation:original", []int64{a})
	check("relation:derivative", []int64{c})
	check("relation:source", []int64{a})
	check("relation:any", []int64{a, b, c})
	check("relation:none", []int64{d})

	if _, err := database.Write.Exec(`UPDATE images SET series = 'My Set' WHERE id = ?`, d); err != nil {
		t.Fatal(err)
	}
	check("relation:collection", []int64{d})
	check("relation:any", []int64{a, b, c, d})
	check("relation:none", []int64{})
}

func TestExecute_IDFilter(t *testing.T) {
	database, env := setupSearchDB(t)
	a := ingestTestImageWithID(t, database, env, "a.png")
	b := ingestTestImageWithID(t, database, env, "b.png")
	_ = b

	expr, err := Parse("id:" + strconv.FormatInt(a, 10))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, err := Execute(database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Results) != 1 || res.Results[0].ID != a {
		t.Fatalf("id:%d got %v, want [%d]", a, res.Results, a)
	}

	bad, err := Parse("id:nope")
	if err != nil {
		t.Fatal(err)
	}
	bres, err := Execute(database, Query{Expr: bad, Sort: "newest", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if bres.Total != 0 {
		t.Fatalf("id:nope total=%d, want 0", bres.Total)
	}
}

// ingestTestImageWithID is a convenience around ingestTestImage that
// returns the ingested image's primary key.
func ingestTestImageWithID(t *testing.T, database *db.DB, env *searchEnv, name string) int64 {
	t.Helper()
	ingestTestImage(t, database, env, name)
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestExecute_PhashMalformed(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	for _, q := range []string{"phash:nothex", "phash:abc~99", "phash:abcd"} {
		expr, err := Parse(q)
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		res, err := Execute(database, Query{Expr: expr, Sort: "newest", Page: 1, Limit: 40})
		if err != nil {
			t.Fatalf("execute %q: %v", q, err)
		}
		if res.Total != 0 {
			t.Fatalf("%q matched %d rows, want 0", q, res.Total)
		}
	}
}

