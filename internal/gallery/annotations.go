package gallery

import (
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// AnnotationsForImage returns every positional note overlaid on imageID.
func AnnotationsForImage(database *db.DB, imageID int64) ([]models.Annotation, error) {
	rows, err := database.Read.Query(
		`SELECT site, post_id, x, y, w, h, body FROM image_annotations WHERE image_id = ? ORDER BY id`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.Annotation
	for rows.Next() {
		var a models.Annotation
		if err := rows.Scan(&a.Site, &a.PostID, &a.X, &a.Y, &a.W, &a.H, &a.Body); err != nil {
			return nil, err
		}
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
		`DELETE FROM image_annotations WHERE image_id = ? AND site = ? AND post_id = ?`,
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
