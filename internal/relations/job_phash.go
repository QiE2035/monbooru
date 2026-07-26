package relations

import (
	"context"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
)

// PhashBackfillProgress is the per-image progress callback the
// caller-side job manager uses. processed / total drive the status-bar
// progress bar; the helper stays decoupled from internal/jobs so it can
// be tested without the manager.
type PhashBackfillProgress func(processed, total int, message string)

// BackfillPhashes computes images.phash for every non-missing row that
// currently has NULL. Reads the candidate id list up front so the read
// cursor doesn't stay open for the duration of the (potentially long)
// job; each thumbnail load + DCT + UPDATE runs as one write.
//
// The function honours ctx cancellation between rows and returns the
// (processed, updated) count via the final progress call so the caller
// can summarise the run.
func BackfillPhashes(ctx context.Context, database *db.DB, thumbnailsPath string, progress PhashBackfillProgress) (processed, updated int, err error) {
	ids, err := db.QueryIDs(database.Read,
		`SELECT id FROM images WHERE phash IS NULL AND is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}

	total := len(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return processed, updated, ctx.Err()
		}
		if progress != nil {
			progress(processed, total, "")
		}
		if err := gallery.RecomputeAndStorePhash(ctx, database, id, thumbnailsPath); err != nil {
			// Decode / thumbnail-missing failures are common during a
			// backfill (rebuild-thumbs hasn't run, the row is for a video
			// whose thumbnail never generated). Log at debug and move on;
			// the row stays at NULL and the operator can retry after
			// rebuilding thumbnails.
			logx.Debugf("phash backfill image %d: %v", id, err)
			processed++
			continue
		}
		processed++
		updated++
	}
	if progress != nil {
		progress(processed, total, "")
	}
	return processed, updated, nil
}
