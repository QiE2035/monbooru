package search

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestParse_Filter_Source(t *testing.T) {
	e, _ := Parse("source:sd")
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "source" || f.Val != "sd" {
		t.Errorf("parse source:sd failed")
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

func TestBuildWhere_AnimatedTrue(t *testing.T) {
	expr := FilterExpr{Key: "animated", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "file_type IN ('gif', 'mp4', 'webm')") {
		t.Errorf("where clause missing animated set: %s", where)
	}
}

func TestBuildWhere_AnimatedFalse(t *testing.T) {
	expr := FilterExpr{Key: "animated", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "file_type NOT IN ('gif', 'mp4', 'webm')") {
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
// shape: AndExpr{userExpr, ceilingChain}. Without the wrap-aware path
// fastCountCeiling rejects, the COUNT walks every visible image, and
// every search the user types under an active rating ceiling pays the
// slow path.
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

	// Ceiling = sensitive: drop questionable+explicit. User searched for
	// blue_eyes. Expected match: just safeBlueID. Both bounds in play:
	// blue_eyes carriers = 2; chain bound = visible(4) - hidden(1) = 3.
	// min(2, 3) = 2 (upper bound, exact for this shape since the only
	// blue_eyes image hidden by the chain is expBlueID).
	expr := AndExpr{
		Left: TagExpr{Tag: "blue_eyes"},
		Right: AndExpr{
			Left:  NotExpr{Expr: FilterExpr{Key: "rating", Val: "questionable"}},
			Right: NotExpr{Expr: FilterExpr{Key: "rating", Val: "explicit"}},
		},
	}
	got, ok := fastCountCeiling(database, expr)
	if !ok {
		t.Fatal("fastCountCeiling should recognise the wrapped ceiling AST")
	}
	if got != 2 {
		t.Errorf("fastCountCeiling = %d, want 2 (min of blue_eyes=2 and chain=3)", got)
	}

	// And Execute end-to-end resolves to the actual single match.
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].ID != safeBlueID {
		t.Errorf("Execute results = %+v, want only safeBlueID=%d", result.Results, safeBlueID)
	}
}

// TestFastCountCeiling_WrappedNoFastBound covers an AndExpr{userExpr,
// chain} where userExpr is a shape fastTagTotal can't bound (a fav:
// filter here). The chain bound alone is still a valid upper bound.
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
	got, ok := fastCountCeiling(database, expr)
	if !ok {
		t.Fatal("fastCountCeiling should fall back to chain bound for unbounded userExpr")
	}
	if got != 2 {
		t.Errorf("fastCountCeiling = %d, want 2 (chain bound: visible 3 - explicit carrier 1)", got)
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

// TestFastCountSource_Csv covers the comma-separated form: source_type
// values matching the 4-LIKE pattern get summed via index-pinned
// COUNT(*) WHERE source_type IN (...). Same match set as the slow
// path; only the count query restructures.
func TestFastCountSource_Csv(t *testing.T) {
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
		{"ai", 0, false},
		{"none", 0, false},
		{"sd", 0, false},
		// CSV value matching nothing returns exact 0.
		{"x,y", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			got, ok := fastCountSource(database, FilterExpr{Key: "source", Val: tc.val})
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFastCountTagged_UpperBound verifies the fast-path returns the
// visible-image count for tagged:true / autotagged:true. The bound is
// exact when every visible image is tagged (the audit fixture's state
// and any synced library's typical state) and over-shoots for fresh
// imports - same trade-off the wildcard / AND helpers already accept.
// The :false variants fall through so untagged-triage queries keep
// their exact slow-path count.
func TestFastCountTagged_UpperBound(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "ft_a.png")
	ingestTestImage(t, database, env, "ft_b.png")

	cases := []struct {
		key  string
		val  string
		want int
		ok   bool
	}{
		{"tagged", "true", 2, true},
		{"autotagged", "true", 2, true},
		// :false stays on the slow path so the count is exact.
		{"tagged", "false", 0, false},
		{"autotagged", "false", 0, false},
		// Other keys never enter this helper.
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
		{"source filter", FilterExpr{Key: "source", Val: "ai"}, false},
		{"folder filter", FilterExpr{Key: "folder", Val: "anime"}, false},
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
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('alpha', ?, 5)`, generalID,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, false); ok {
		t.Error("single literal at root should not engage the driver under indexed sort")
	}
	// Random sort is the override: single literal does engage so the
	// materialised set bounds the temp-sort input.
	if _, ok := pickAndDriverTag(database, TagExpr{Tag: "alpha"}, true); !ok {
		t.Error("single literal at root should engage the driver under random sort")
	}
}

// TestExecute_RandomSortSingleTagDriver pins F006: a random-sort query
// with a single literal tag predicate engages the AND-driver so the
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

// TestPickAndDriverTag_PopularIntersect pins the F001 path: every leaf
// of an AND chain has usage_count above andDriverThreshold, so the
// driver returns multiple legs that the caller will INTERSECT-bound.
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

	// Three popular leaves AND'd: every leg above threshold, all three
	// legs returned for INTERSECT.
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
	if len(legs) != 3 {
		t.Errorf("legs = %d, want 3 (one per ANDed leaf)", len(legs))
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

func TestBuildOrder_Unknown(t *testing.T) {
	// Unknown sorts fall back to newest (ingested_at DESC)
	got := buildOrder("unknown_sort", "", 0)
	if !strings.Contains(got, "ingested_at") || !strings.Contains(got, "DESC") {
		t.Errorf("unknown sort: %q", got)
	}
}

func TestBuildOrder_Random(t *testing.T) {
	got := buildOrder("random", "", 12345)
	if !strings.Contains(got, "12345") {
		t.Errorf("random: expected seed in order clause, got %q", got)
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

func TestBuildWhere_Source(t *testing.T) {
	// "sd" is aliased to "a1111"; source filter uses 4 LIKE args for comma-separated types.
	expr := FilterExpr{Key: "source", Val: "sd"}
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

func TestBuildWhere_SourceAI(t *testing.T) {
	// "ai" expands inline to match a1111, comfyui, and the combined source type;
	// no bound args are needed because the values are inlined into the SQL.
	expr := FilterExpr{Key: "source", Val: "ai"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for source:ai, got %v", args)
	}
	for _, want := range []string{"'a1111'", "'comfyui'", "'a1111,comfyui'"} {
		if !strings.Contains(where, want) {
			t.Errorf("where = %q, missing %s", where, want)
		}
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

func TestBuildWhere_UnknownFilter(t *testing.T) {
	// Unknown keys with a non-empty value are treated as category-qualified tag searches.
	expr := FilterExpr{Key: "bogus", Val: "val"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "EXISTS") {
		t.Errorf("unknown key:val should yield category-qualified EXISTS clause, got %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args (tag name + category name), got %d: %v", len(args), args)
	}

	// Unknown key with empty value → 1=1
	expr2 := FilterExpr{Key: "bogus", Val: ""}
	where2, _, _ := buildWhere(expr2)
	if where2 != "1=1" {
		t.Errorf("unknown filter with empty val should yield 1=1, got %q", where2)
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
	// When the prefix IS a real category name, the DB-aware builder must
	// keep the old "category-qualified" behaviour so `artist:foo` still
	// searches for the tag `foo` in the `artist` category.
	database, _ := setupSearchDB(t)

	expr := FilterExpr{Key: "artist", Val: "foo"}
	where, args, _ := buildWhereDB(expr, database)
	if !strings.Contains(where, "tc.name = ?") {
		t.Errorf("category-qualified branch should reference tc.name, got: %q", where)
	}
	if len(args) != 2 || args[0] != "foo" || args[1] != "artist" {
		t.Errorf("args = %v, want [foo artist]", args)
	}
}

func TestBuildDateFilter_After(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter(">2024-01-01")
	if !strings.Contains(clause, "> ?") || b.args[0] != "2024-01-01" {
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
	// Unknown filter produces "1=1" - AND should still work
	expr := AndExpr{
		Left:  FilterExpr{Key: "bogus", Val: ""},
		Right: TagExpr{Tag: "cute"},
	}
	_, args, _ := buildWhere(expr)
	// Should have 1 arg for the tag search
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %d: %v", len(args), args)
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
	// bucket. Images outside the current image's bucket must not appear
	// as prev/next, even if they match the predicate.
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
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('rndtag', ?, 3) RETURNING id`,
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

// TestExecuteAdjacent_NewestSparseAndBucketBound pins F002: a 3-AND
// back_q under newest sort gates prev/next to an id-window so a sparse
// intersection late in the result set can't force a multi-second scan.
// The 2-AND shape keeps the pre-existing unbounded behaviour because
// the cursor walk is acceptable there.
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
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 3) RETURNING id`,
			name, generalID,
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

