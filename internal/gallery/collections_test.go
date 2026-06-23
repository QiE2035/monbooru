package gallery

import (
	"database/sql"
	"sort"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
)

func newCollectionsTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func insertImage(t *testing.T, database *db.DB, missing bool) int64 {
	t.Helper()
	m := 0
	if missing {
		m = 1
	}
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, is_missing) VALUES (?,?,?,?,?)`,
		randSHA(t), "/x.png", "image", 1, m)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

var shaCounter int

func randSHA(t *testing.T) string {
	t.Helper()
	shaCounter++
	return fmtSHA(shaCounter)
}

func fmtSHA(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hex[(n+i)%16]
	}
	b[0] = hex[n%16]
	return string(b)
}

func member(t *testing.T, database *db.DB, imageID int64, name string) {
	t.Helper()
	if _, err := database.Write.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?,?,NULL)`, imageID, name); err != nil {
		t.Fatal(err)
	}
}

func tagImage(t *testing.T, database *db.DB, imageID, tagID int64) {
	t.Helper()
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (?,?)`, imageID, tagID); err != nil {
		t.Fatal(err)
	}
}

func ratingTagID(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedCollectionsFixture builds a fixture covering the visibility and
// ceiling edges the listing/count queries must honour:
//   - alpha:   3 visible members
//   - beta:    2 members, both missing      (drops off entirely)
//   - gamma:   1 visible + 1 missing         (count 1)
//   - delta:   2 visible, both rated explicit
//   - epsilon: 1 visible plain + 1 visible rated explicit
func seedCollectionsFixture(t *testing.T, database *db.DB) (explicitID int64) {
	t.Helper()
	explicitID = ratingTagID(t, database, "explicit")
	for i := 0; i < 3; i++ {
		member(t, database, insertImage(t, database, false), "alpha")
	}
	for i := 0; i < 2; i++ {
		member(t, database, insertImage(t, database, true), "beta")
	}
	member(t, database, insertImage(t, database, false), "gamma")
	member(t, database, insertImage(t, database, true), "gamma")
	for i := 0; i < 2; i++ {
		id := insertImage(t, database, false)
		tagImage(t, database, id, explicitID)
		member(t, database, id, "delta")
	}
	member(t, database, insertImage(t, database, false), "epsilon")
	ex := insertImage(t, database, false)
	tagImage(t, database, ex, explicitID)
	member(t, database, ex, "epsilon")
	return explicitID
}

func TestCountCollections(t *testing.T) {
	database := newCollectionsTestDB(t)
	explicitID := seedCollectionsFixture(t, database)

	cases := []struct {
		name    string
		filter  string
		exclude []int64
		want    int
	}{
		{"unfiltered drops the all-missing label", "", nil, 4},
		{"ceiling drops a fully-explicit label", "", []int64{explicitID}, 3},
		{"substring filter", "alph", nil, 1},
		{"no match", "zzz", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CountCollections(database, tc.filter, tc.exclude)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("CountCollections(%q, %v) = %d, want %d", tc.filter, tc.exclude, got, tc.want)
			}
		})
	}
}

func TestListCollections(t *testing.T) {
	database := newCollectionsTestDB(t)
	explicitID := seedCollectionsFixture(t, database)

	names := func(list []CollectionSummary) []string {
		out := make([]string, len(list))
		for i, c := range list {
			out[i] = c.Name
		}
		return out
	}
	countOf := func(list []CollectionSummary, name string) int {
		for _, c := range list {
			if c.Name == name {
				return c.Count
			}
		}
		return -1
	}

	list, err := ListCollections(database, "", "name", 60, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(list), []string{"alpha", "delta", "epsilon", "gamma"}; !equalStrings(got, want) {
		t.Fatalf("name sort = %v, want %v", got, want)
	}
	if c := countOf(list, "gamma"); c != 1 {
		t.Fatalf("gamma visible count = %d, want 1 (one member missing)", c)
	}
	if c := countOf(list, "alpha"); c != 3 {
		t.Fatalf("alpha visible count = %d, want 3", c)
	}

	// Size sort: alpha (3) leads, gamma (1) trails.
	bySize, err := ListCollections(database, "", "size", 60, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := names(bySize); s[0] != "alpha" || s[len(s)-1] != "gamma" {
		t.Fatalf("size sort = %v, want alpha first and gamma last", s)
	}

	// Ceiling drops delta (all explicit) and trims epsilon to its plain member.
	underCeiling, err := ListCollections(database, "", "name", 60, 0, []int64{explicitID})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(underCeiling), []string{"alpha", "epsilon", "gamma"}; !equalStrings(got, want) {
		t.Fatalf("ceiling list = %v, want %v", got, want)
	}
	if c := countOf(underCeiling, "epsilon"); c != 1 {
		t.Fatalf("epsilon under ceiling = %d, want 1", c)
	}
}

func homeOf(t *testing.T, database *db.DB, imageID int64) string {
	t.Helper()
	var s sql.NullString
	if err := database.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func homeOrderOf(t *testing.T, database *db.DB, imageID int64) (int, bool) {
	t.Helper()
	var o sql.NullInt64
	if err := database.Read.QueryRow(`SELECT series_order FROM images WHERE id = ?`, imageID).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return int(o.Int64), o.Valid
}

// TestCollectionMembershipMirror exercises the series/series_order home
// mirror across the add/remove lifecycle: first add adopts the home,
// re-adding the home updates its order, removing the home promotes the
// next membership, and removing the last clears the mirror.
func TestCollectionMembershipMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)

	o1, o5, o2 := 1, 5, 2
	if err := AddCollectionMembership(database, id, "A", &o1); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "A" {
		t.Fatalf("home after first add = %q, want A (adopted)", got)
	}
	if v, ok := homeOrderOf(t, database, id); !ok || v != 1 {
		t.Fatalf("series_order after first add = (%d,%v), want 1", v, ok)
	}

	if err := AddCollectionMembership(database, id, "A", &o5); err != nil {
		t.Fatal(err)
	}
	if v, ok := homeOrderOf(t, database, id); !ok || v != 5 {
		t.Fatalf("series_order after re-add = (%d,%v), want 5 (mirror tracks home order)", v, ok)
	}

	if err := AddCollectionMembership(database, id, "B", &o2); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "A" {
		t.Fatalf("home after adding extra = %q, want A (unchanged)", got)
	}
	assertMemberships(t, database, id, "A", "B")

	if err := RemoveCollectionMembership(database, id, "A"); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "B" {
		t.Fatalf("home after removing A = %q, want B (promoted)", got)
	}
	assertMemberships(t, database, id, "B")

	if err := RemoveCollectionMembership(database, id, "B"); err != nil {
		t.Fatal(err)
	}
	if got := homeOf(t, database, id); got != "" {
		t.Fatalf("home after removing last = %q, want empty (mirror cleared)", got)
	}
	assertMemberships(t, database, id)
}

func assertMemberships(t *testing.T, database *db.DB, imageID int64, want ...string) {
	t.Helper()
	cols, err := CollectionsForImage(database, imageID)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(cols))
	for i, c := range cols {
		got[i] = c.Name
	}
	sort.Strings(got)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("memberships = %v, want %v", got, want)
	}
}

// TestSetHomeCollection_PromoteExistingMembership guards against the home
// pointer move silently dropping another collection the image is filed
// under.
func TestSetHomeCollection_PromoteExistingMembership(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	one, five := 1, 5
	if err := SetHomeCollection(database, id, "A", &one); err != nil {
		t.Fatal(err)
	}
	if err := AddCollectionMembership(database, id, "B", &five); err != nil {
		t.Fatal(err)
	}

	// Pointing the home at the existing extra membership keeps both.
	if err := SetHomeCollection(database, id, "B", nil); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "A", "B")
	if got := homeOf(t, database, id); got != "B" {
		t.Fatalf("home after promote = %q, want B", got)
	}

	// Relabelling onto a brand-new name still drops the former home.
	if err := SetHomeCollection(database, id, "C", nil); err != nil {
		t.Fatal(err)
	}
	assertMemberships(t, database, id, "A", "C")
	if got := homeOf(t, database, id); got != "C" {
		t.Fatalf("home after relabel = %q, want C", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
