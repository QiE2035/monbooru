package gallery

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// images.series / series_order mirror one "home" membership so the
// global order-sort and the adjacency cursor keep riding the scalar
// columns. The invariant: series != '' iff the image has at least one
// membership, and when set it equals the home row in image_collections.
// The helpers below maintain that invariant.

func orderValue(order *int) any {
	if order == nil {
		return nil
	}
	return *order
}

// CollectionsForImage returns every membership of imageID, ordered as the
// detail page renders them: positioned rows first (ascending), then the
// unordered ones by name.
func CollectionsForImage(database *db.DB, imageID int64) ([]models.Collection, error) {
	rows, err := database.Read.Query(
		`SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.Collection
	for rows.Next() {
		var c models.Collection
		var pos sql.NullInt64
		if err := rows.Scan(&c.Name, &pos); err != nil {
			return nil, err
		}
		if pos.Valid {
			v := int(pos.Int64)
			c.Order = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddCollectionMembership upserts a membership (adding it or just updating
// its position) and keeps the home mirror in step: an image with no home
// adopts this one; re-setting the home's own position updates the mirror.
func AddCollectionMembership(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	switch {
	case home == "":
		if _, err := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
			name, orderValue(order), imageID); err != nil {
			return err
		}
	case strings.EqualFold(home, name):
		if _, err := tx.Exec(`UPDATE images SET series_order = ? WHERE id = ?`,
			orderValue(order), imageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveCollectionMembership drops a membership; when it was the home the
// next membership is promoted (or the mirror cleared if none remain).
func RemoveCollectionMembership(database *db.DB, imageID int64, name string) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
		imageID, name); err != nil {
		return err
	}
	if err := rebindHomeTx(tx, imageID, name); err != nil {
		return err
	}
	return tx.Commit()
}

// SetHomeCollection points imageID's home at name with the given order,
// renaming or clearing the previous home and keeping image_collections in
// sync. Used by the API and ingest, which carry a single collection field.
// An empty name clears the home, promoting another membership if one is
// left so the series != ” invariant holds. Pointing the home at a label
// the image already belongs to promotes that membership in place and
// leaves the former home as an extra; only relabelling onto a new name
// (or clearing) drops the old home.
func SetHomeCollection(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	oldHome, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	relabel := oldHome != "" && !strings.EqualFold(oldHome, name)
	if relabel && name != "" {
		var x int
		switch err := tx.QueryRow(
			`SELECT 1 FROM image_collections WHERE image_id = ? AND name = ?`, imageID, name).Scan(&x); {
		case err == nil:
			relabel = false // target already a member: promote, don't drop the old home
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if relabel {
		if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
			imageID, oldHome); err != nil {
			return err
		}
	}
	if name == "" {
		if err := rebindHomeTx(tx, imageID, oldHome); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name, orderValue(order), imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func homeName(tx *sql.Tx, imageID int64) (string, error) {
	var home sql.NullString
	if err := tx.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !home.Valid {
		return "", nil
	}
	return home.String, nil
}

// rebindHomeTx repoints the mirror after changedName left the membership
// set. A no-op unless changedName was the home; then it promotes the next
// membership or clears the mirror when none remain.
func rebindHomeTx(tx *sql.Tx, imageID int64, changedName string) error {
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	if home == "" || !strings.EqualFold(home, changedName) {
		return nil
	}
	var name sql.NullString
	var pos sql.NullInt64
	err = tx.QueryRow(
		`SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name LIMIT 1`, imageID).Scan(&name, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET series = '', series_order = NULL WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	var ord any
	if pos.Valid {
		ord = pos.Int64
	}
	_, e := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name.String, ord, imageID)
	return e
}

// CollectionSummary is one row of the collections management page: a
// label, its visible member count, and a few members for the preview.
type CollectionSummary struct {
	Name    string
	Count   int
	Samples []CollectionSample
}

// CollectionSample is one preview tile: the image id and its position
// within the collection (nil when the membership is unordered).
type CollectionSample struct {
	ID    int64
	Order *int
}

// collectionFilterWhere returns the substring-match fragment (and its
// args) for a non-empty name filter against col, empty otherwise so it
// splices into a WHERE clause without juggling the boundary.
func collectionFilterWhere(col, nameFilter string) (string, []any) {
	if nameFilter == "" {
		return "", nil
	}
	return ` AND ` + col + ` LIKE ? ESCAPE '\'`, []any{"%" + db.EscapeLike(nameFilter) + "%"}
}

// ListCollections returns one page of collection labels with their
// visible (non-missing) member counts. sort "name" orders alphabetically;
// any other value orders by member count descending, name as tiebreaker.
// Members carrying a tag in excludeIDs (the rating ceiling) drop from the
// count, so a collection with no visible member left falls off the page.
func ListCollections(database *db.DB, nameFilter, sort string, limit, offset int, excludeIDs []int64) ([]CollectionSummary, error) {
	exclude, args := excludeNotExists("c.image_id", excludeIDs)
	where, filterArgs := collectionFilterWhere("c.name", nameFilter)
	args = append(args, filterArgs...)
	orderBy := "cnt DESC, c.name ASC"
	if sort == "name" {
		orderBy = "c.name ASC"
	}
	args = append(args, limit, offset)
	// EXISTS visibility (vs a join to images) lets the GROUP BY stream off
	// idx_image_collections_name instead of a temp B-tree over every member.
	rows, err := database.Read.Query(
		`SELECT c.name, COUNT(*) cnt FROM image_collections c
		 WHERE EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0)`+exclude+where+`
		 GROUP BY c.name ORDER BY `+orderBy+` LIMIT ? OFFSET ?`,
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CollectionSummary
	for rows.Next() {
		var c CollectionSummary
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountCollections returns the number of distinct collection labels with
// at least one visible member, honoring the same substring filter and the
// rating ceiling (excludeIDs).
func CountCollections(database *db.DB, nameFilter string, excludeIDs []int64) (int, error) {
	exclude, args := excludeNotExists("c.image_id", excludeIDs)
	where, filterArgs := collectionFilterWhere("d.name", nameFilter)
	args = append(args, filterArgs...)
	// Enumerate distinct labels off the name index and keep those with a
	// visible, ceiling-clear member; the per-label EXISTS short-circuits, so
	// cost tracks the label count, not the membership count.
	var n int
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT name FROM image_collections) d
		 WHERE EXISTS (SELECT 1 FROM image_collections c JOIN images i ON i.id = c.image_id
		   WHERE c.name = d.name AND i.is_missing = 0`+exclude+`)`+where, args...).Scan(&n)
	return n, err
}

// CollectionSamples returns up to per visible members for each named
// collection, in reading order (position first with NULLs last, then id).
// Members above the rating ceiling (excludeIDs) are skipped so the preview
// matches the listing. The map is keyed by lower-cased label so a single
// key survives images that stored the same NOCASE label in different cases.
func CollectionSamples(database *db.DB, names []string, per int, excludeIDs []int64) (map[string][]CollectionSample, error) {
	if len(names) == 0 || per <= 0 {
		return map[string][]CollectionSample{}, nil
	}
	exclude, args := excludeNotExists("i.id", excludeIDs)
	placeholders, nameArgs := db.InPlaceholders(names)
	args = append(args, nameArgs...)
	args = append(args, per)
	rows, err := database.Read.Query(
		`SELECT name, image_id, position FROM (
		   SELECT c.name AS name, c.image_id AS image_id, c.position AS position,
		          ROW_NUMBER() OVER (PARTITION BY c.name
		             ORDER BY c.position IS NULL, c.position, c.image_id) AS rn
		   FROM image_collections c JOIN images i ON i.id = c.image_id
		   WHERE i.is_missing = 0`+exclude+` AND c.name IN (`+placeholders+`)
		 ) WHERE rn <= ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]CollectionSample, len(names))
	for rows.Next() {
		var name string
		var s CollectionSample
		var pos sql.NullInt64
		if err := rows.Scan(&name, &s.ID, &pos); err != nil {
			return out, err
		}
		if pos.Valid {
			v := int(pos.Int64)
			s.Order = &v
		}
		key := strings.ToLower(name)
		out[key] = append(out[key], s)
	}
	return out, rows.Err()
}

// CollectionMemberIDs returns every image id filed under name (case-
// insensitive), missing rows included, so a rename or dissolve reaches
// the whole collection rather than only its visible members.
func CollectionMemberIDs(database *db.DB, name string) ([]int64, error) {
	rows, err := database.Read.Query(
		`SELECT image_id FROM image_collections WHERE name = ? COLLATE NOCASE`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return db.ScanIDs(rows)
}
