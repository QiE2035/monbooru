package gallery

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// images.source / images.url mirror the "primary" origin - the oldest row in
// image_sources - so the search executor, the image response, and gallery
// export keep riding the scalar columns. The invariant: they are non-empty
// iff the image has at least one origin, and when set they equal the primary
// row. The helpers below maintain it, the same way the collections helpers
// maintain images.series.

// SourcesForImage returns every origin of imageID, primary (oldest) first.
func SourcesForImage(database *db.DB, imageID int64) ([]models.ImageSource, error) {
	rows, err := database.Read.Query(
		`SELECT site, post_id, url, commentary FROM image_sources WHERE image_id = ? ORDER BY rowid`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []models.ImageSource
	for rows.Next() {
		var s models.ImageSource
		if err := rows.Scan(&s.Site, &s.PostID, &s.URL, &s.Commentary); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AddSourceMembership upserts one origin (adding it or updating its url,
// keyed by site+post_id) and rebinds the primary mirror. An empty incoming
// url never clears a stored one - a url-less re-push or enrich of a known
// origin must not wipe operator-entered or previously-fetched provenance,
// matching the commentary path's empty-guard.
func AddSourceMembership(database *db.DB, imageID int64, site, postID, url string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
		 ON CONFLICT(image_id, site, post_id) DO UPDATE SET
		   url = CASE WHEN excluded.url != '' THEN excluded.url ELSE url END`,
		imageID, site, postID, url); err != nil {
		return err
	}
	if err := rebindPrimarySourceTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSourceMD5 records the md5 the source claimed for one origin (the audit
// trail; never a dedup key). An empty incoming value keeps the stored one.
func SetSourceMD5(database *db.DB, imageID int64, site, postID, md5 string) error {
	md5 = strings.TrimSpace(md5)
	if md5 == "" {
		return nil
	}
	_, err := database.Write.Exec(
		`UPDATE image_sources SET md5 = ? WHERE image_id = ? AND site = ? AND post_id = ?`,
		md5, imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// RenameSourceMembership relabels one origin in place, keeping its
// commentary / md5 / fetched_at and its age (a relabelled primary stays
// primary), and re-keys the origin's annotations so they follow the new
// identity. When the new identity already exists on the image the two rows
// merge: the target keeps its own commentary unless it has none, and the
// old row is dropped. A missing prev identity falls back to a plain upsert.
func RenameSourceMembership(database *db.DB, imageID int64, prevSite, prevPost, site, postID, url string) error {
	prevSite = strings.TrimSpace(prevSite)
	prevPost = strings.TrimSpace(prevPost)
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var prevRid int64
	switch err := tx.QueryRow(
		`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, prevSite, prevPost).Scan(&prevRid); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(
			`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, site, post_id) DO UPDATE SET url = excluded.url`,
			imageID, site, postID, url); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		var targetRid int64
		switch err := tx.QueryRow(
			`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID).Scan(&targetRid); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`UPDATE image_sources SET site = ?, post_id = ?, url = ? WHERE rowid = ?`,
				site, postID, url, prevRid); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(
				`UPDATE image_sources SET url = ?,
				        commentary = CASE WHEN commentary = '' THEN (SELECT commentary FROM image_sources WHERE rowid = ?) ELSE commentary END
				 WHERE rowid = ?`,
				url, prevRid, targetRid); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, prevRid); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE image_annotations SET site = ?, post_id = ? WHERE image_id = ? AND site = ? AND post_id = ?`,
			site, postID, imageID, prevSite, prevPost); err != nil {
			return err
		}
	}
	if err := rebindPrimarySourceTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveSourceMembership drops one origin along with the annotations it
// pulled (they carry the same identity and have no other removal path) and
// rebinds the primary mirror.
func RemoveSourceMembership(database *db.DB, imageID int64, site, postID string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, site, postID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM image_annotations WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, site, postID); err != nil {
		return err
	}
	if err := rebindPrimarySourceTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSourceCommentary sets the artist commentary attributed to one origin. A
// non-empty body upserts the origin (creating it when absent, so commentary can
// be added to a source the image does not list yet); an empty body only clears
// an existing origin's commentary and never creates one. Rebinds the primary
// mirror since an upsert can add the first origin.
func SetSourceCommentary(database *db.DB, imageID int64, site, postID, commentary string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	commentary = strings.TrimSpace(commentary)
	if site == "" {
		return errors.New("source label required")
	}
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if commentary == "" {
		if _, err := tx.Exec(
			`UPDATE image_sources SET commentary = '' WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(
		`INSERT INTO image_sources (image_id, site, post_id, commentary) VALUES (?, ?, ?, ?)
		 ON CONFLICT(image_id, site, post_id) DO UPDATE SET commentary = excluded.commentary`,
		imageID, site, postID, commentary); err != nil {
		return err
	}
	if err := rebindPrimarySourceTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrSourceIdentityExists reports a relabel that would collide with another
// origin already recorded on the image.
var ErrSourceIdentityExists = errors.New("another source with that label already exists on this image")

// SetPrimarySource edits the primary origin in place - the site / url the
// scalar mirrors - or clears it when both are empty, creating a first origin
// when the image has none. Used by the API PATCH, which carries a single
// source/url pair. Operating on the primary row's rowid keeps its age so it
// stays primary through a relabel.
func SetPrimarySource(database *db.DB, imageID int64, site, url string) error {
	site = strings.TrimSpace(site)
	url = strings.TrimSpace(url)
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var rid int64
	var curSite, curPost string
	err = tx.QueryRow(`SELECT rowid, site, post_id FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&rid, &curSite, &curPost)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if site != "" || url != "" {
			if _, err := tx.Exec(
				`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, '', ?)`,
				imageID, site, url); err != nil {
				return err
			}
		}
	case err != nil:
		return err
	case site == "" && url == "":
		if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, rid); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM image_annotations WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, curSite, curPost); err != nil {
			return err
		}
	default:
		var clash bool
		if err := tx.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ? AND rowid != ?)`,
			imageID, site, curPost, rid).Scan(&clash); err != nil {
			return err
		}
		if clash {
			return ErrSourceIdentityExists
		}
		if _, err := tx.Exec(`UPDATE image_sources SET site = ?, url = ? WHERE rowid = ?`,
			site, url, rid); err != nil {
			return err
		}
	}
	if err := rebindPrimarySourceTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

// rebindPrimarySourceTx repoints images.source / images.url at the oldest
// origin row (or clears them when none remain).
func rebindPrimarySourceTx(tx *sql.Tx, imageID int64) error {
	var site, url string
	err := tx.QueryRow(`SELECT site, url FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&site, &url)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET source = '', url = '' WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	_, e := tx.Exec(`UPDATE images SET source = ?, url = ? WHERE id = ?`, site, url, imageID)
	return e
}
