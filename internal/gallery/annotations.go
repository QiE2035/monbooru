package gallery

import (
	"errors"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// AnnotationsForImage returns every positional note overlaid on imageID,
// carrying the row id and manual flag so the editable list can address a box
// and distinguish operator-drawn boxes from source-pulled ones.
func AnnotationsForImage(database *db.DB, imageID int64) ([]models.Annotation, error) {
	rows, err := database.Read.Query(
		`SELECT id, site, post_id, x, y, w, h, body, manual FROM image_annotations WHERE image_id = ? ORDER BY id`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.Annotation
	for rows.Next() {
		var a models.Annotation
		var manual int
		if err := rows.Scan(&a.ID, &a.Site, &a.PostID, &a.X, &a.Y, &a.W, &a.H, &a.Body, &manual); err != nil {
			return nil, err
		}
		a.Manual = manual == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReplaceSourceAnnotations sets the annotations attributed to one source to
// exactly boxes, dropping whatever that source contributed before (clone on
// re-pull). An empty boxes clears the source's set and leaves other sources'
// boxes untouched.
func ReplaceSourceAnnotations(database *db.DB, imageID int64, site, postID string, boxes []models.Annotation) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`DELETE FROM image_annotations WHERE image_id = ? AND site = ? AND post_id = ? AND manual = 0`,
		imageID, site, postID); err != nil {
		return err
	}
	for _, b := range boxes {
		if _, err := tx.Exec(
			`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			imageID, site, postID, b.X, b.Y, b.W, b.H, b.Body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddManualAnnotation stores an operator-drawn box (manual = 1, no source
// identity). Coordinates are the caller's already-validated original-image
// pixels; body is plain text.
func AddManualAnnotation(database *db.DB, imageID int64, x, y, w, h int, body string) error {
	_, err := database.Write.Exec(
		`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual) VALUES (?, '', '', ?, ?, ?, ?, ?, 1)`,
		imageID, x, y, w, h, body)
	return err
}

// UpdateAnnotation edits one box by id, source-pulled or operator-drawn, keeping
// its manual flag. An edit to a source box is overwritten by a later re-pull,
// the same rule commentary follows.
func UpdateAnnotation(database *db.DB, id int64, x, y, w, h int, body string) error {
	res, err := database.Write.Exec(
		`UPDATE image_annotations SET x = ?, y = ?, w = ?, h = ?, body = ? WHERE id = ?`,
		x, y, w, h, body, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("annotation not found")
	}
	return nil
}

// DeleteAnnotation removes one box by id, source-pulled or operator-drawn. A
// re-pull of the source re-adds a deleted source box; the bulk source-keyed
// replace / removal paths still gate on manual = 0, so they never touch an
// operator box.
func DeleteAnnotation(database *db.DB, id int64) error {
	res, err := database.Write.Exec(
		`DELETE FROM image_annotations WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("annotation not found")
	}
	return nil
}
