package tagger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[TagKey]Scored, taggerNames []string, ratingCatID int64,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve each desired tag to a tag_id, creating new rows as
	// needed. Alias rows redirect to their canonical so we never
	// attach an alias to an image (matches GetOrCreateTag). Two labels
	// that collapse onto the same canonical keep the higher score.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(merged))
	for k, s := range merged {
		var tagID int64
		var isAlias int
		var canonicalID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, is_alias, canonical_tag_id FROM tags WHERE name = ? AND category_id = ?`, k.Name, k.CatID,
		).Scan(&tagID, &isAlias, &canonicalID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count, origin) VALUES (?, ?, 0, ?)`, k.Name, k.CatID, s.TaggerName)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.Name, k.CatID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.Name, k.CatID, err)
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
		}
		if prev, ok := targets[tagID]; !ok || s.Score > prev.score {
			targets[tagID] = target{score: s.Score, taggerName: s.TaggerName}
		}
	}

	type rowInfo struct {
		isAuto     bool
		taggerName string
	}
	current := map[int64]rowInfo{}
	rows, err := tx.QueryContext(ctx,
		`SELECT tag_id, is_auto, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tid int64
		var isAuto int
		var tname sql.NullString
		if err := rows.Scan(&tid, &isAuto, &tname); err != nil {
			_ = rows.Close()
			return err
		}
		current[tid] = rowInfo{isAuto: isAuto == 1, taggerName: tname.String}
	}
	_ = rows.Close()

	toRemove := map[int64]struct{}{}
	if len(taggerNames) > 0 {
		scope := make(map[string]struct{}, len(taggerNames))
		for _, n := range taggerNames {
			scope[n] = struct{}{}
		}
		for tid, info := range current {
			if !info.isAuto {
				continue
			}
			if _, ok := scope[info.taggerName]; !ok {
				continue
			}
			if _, keep := targets[tid]; keep {
				continue
			}
			toRemove[tid] = struct{}{}
		}
	}
	toAdd := map[int64]target{}
	for tid, t := range targets {
		if _, exists := current[tid]; !exists {
			toAdd[tid] = t
		}
	}

	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("remove auto tag %d: %w", tid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("decrement usage for tag %d: %w", tid, err)
		}
	}

	for tid, t := range targets {
		info, exists := current[tid]
		if !exists || !info.isAuto {
			continue
		}
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET confidence = ?, tagger_name = ? WHERE image_id = ? AND tag_id = ? AND is_auto = 1`,
			t.score, tname, imageID, tid); err != nil {
			return fmt.Errorf("refresh attribution for tag %d: %w", tid, err)
		}
	}

	// Every emitted tag records the tagger in the source ledger - the
	// fresh inserts below and the tags already on the image alike, since
	// re-confirming an existing row is what the ledger captures.
	for tid, t := range targets {
		if err := tags.RecordTagSourceTx(tx, imageID, tid, t.taggerName); err != nil {
			return fmt.Errorf("record tag source %d: %w", tid, err)
		}
	}

	for tid, t := range toAdd {
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 1, 0, ?, ?)`,
			imageID, tid, t.score, tname)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET usage_count = usage_count + 1, last_used_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
		if err := tags.ApplyImpliedFanoutTx(tx, imageID, tid, ratingCatID, true); err != nil {
			return fmt.Errorf("fan out implications for tag %d: %w", tid, err)
		}
	}

	// WD14 emits every rating label that beats its threshold, so a
	// single image can pick up `sensitive` and `questionable` in one
	// pass. Sweep lower-rank rating rows so highest-rank wins matches
	// what search resolves to anyway.
	if ratingCatID != 0 {
		hasRating := false
		for k := range merged {
			if k.CatID == ratingCatID {
				hasRating = true
				break
			}
		}
		if hasRating {
			if err := tags.PruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
				return fmt.Errorf("prune lower ratings: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}

	return tx.Commit()
}

// remoteDrainHold is how long the B-side lets the A-side hold a drain
// request waiting for new results. The A-side's server-side wait is
// the same value, so the HTTP response always beats the client
// timeout.
const remoteDrainHold = 5 * time.Second

// remoteJobTimeout is how long a submitted job may stay unanswered
// before the B-side counts it skipped. The A-side GCs its own copy at
// a similar horizon, so this only fires on crashes / partitions and
// guarantees the batch still terminates.
const remoteJobTimeout = 10 * time.Minute

// remoteDefaultWindow is the B-side sliding-window fallback when the
// A-side status probe fails.
const remoteDefaultWindow = 16

// remoteTarget pairs a B-side image id with its canonical path,
// pre-resolved once so the refill loop doesn't re-query per submission.
type remoteTarget struct {
	id   int64
	path string
}

// runRemoteTaggers pushes every eligible image through a paired remote
// tagger using an asynchronous submit / cursor-drain protocol. It
// keeps a sliding window at the A-side's advertised capacity: submit
// up to capacity images, drain completed results, store them, and
// refill - so transfer and inference overlap instead of alternating in
// batches. Draining by cursor is idempotent, so a reconnecting B-side
// resumes without loss or duplication.
func runRemoteTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	remote := newRemoteBackend(cfg.Tagger.RemoteClient.URL, cfg.Tagger.RemoteClient.Token)
	// Remote tagging is the last big heap consumer of the job; give the
	// OS the Go heap back when it finishes so a long batch doesn't stay
	// pinned at its peak RSS.
	defer debug.FreeOSMemory()

	// The window stays at the A-side's advertised capacity so
	// submissions never bounce off a full queue.
	capacity := remoteDefaultWindow
	if cap, _, _, err := remote.Status(ctx); err == nil && cap >= 1 {
		capacity = cap
	}

	// Pre-filter the id list: skip non-static images and rows that
	// can't be looked up, counting each as done+skipped so progress
	// still reaches 100%.
	total := len(ids)
	done := 0
	var skipped int
	remaining := make([]remoteTarget, 0, len(ids))
	for _, id := range ids {
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, id,
		).Scan(&canonPath, &fileType); err != nil {
			skipped++
			done++
			continue
		}
		if !isStaticImageType(fileType) {
			skipped++
			done++
			continue
		}
		// The DB's file_type reflects what ingest accepted; the file
		// itself may still be something the remote tagger cannot use
		// (content swapped after ingest, a HEIC masquerading under a
		// webp name, a missing file). Sniff the header here so such
		// images are skipped locally instead of being pushed to the
		// A-side, which would reject them and burn a retry cycle.
		head, err := readFileHead(canonPath)
		if err != nil {
			logx.Warnf("remote tagger: skip image %d: cannot read file: %v", id, err)
			skipped++
			done++
			continue
		}
		if detectImageContentType(head) == "" {
			logx.Warnf("remote tagger: skip image %d: unsupported content (only jpeg, png, webp, gif accepted)", id)
			skipped++
			done++
			continue
		}
		remaining = append(remaining, remoteTarget{id: id, path: canonPath})
	}

	cursor := int64(0)
	// outstanding maps an A-side job id back to the B-side image id.
	// Draining by cursor is idempotent, so the map also de-duplicates
	// results a reconnected drain would otherwise re-deliver.
	outstanding := map[string]int64{}
	submittedAt := map[string]time.Time{}

	for len(remaining) > 0 || len(outstanding) > 0 {
		if ctx.Err() != nil {
			notifyRemoteCancel(remote, keysOf(outstanding))
			return skipped, ctx.Err()
		}

		progressed := false

		// 1) Drain completed results. Skipped when nothing is in flight
		// so the very first iteration doesn't wait a full drain hold.
		if len(outstanding) > 0 {
			newCursor, results, err := remote.Drain(ctx, cursor, remoteDrainHold)
			if err != nil {
				if ctx.Err() != nil {
					notifyRemoteCancel(remote, keysOf(outstanding))
					return skipped, ctx.Err()
				}
				// A transient drain failure shouldn't abort the batch;
				// retry after a short backoff.
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					notifyRemoteCancel(remote, keysOf(outstanding))
					return skipped, ctx.Err()
				}
				continue
			}
			cursor = newCursor
			for _, r := range results {
				id, ok := outstanding[r.JobID]
				if !ok {
					continue // duplicate from a re-drain
				}
				delete(outstanding, r.JobID)
				delete(submittedAt, r.JobID)
				if r.Err != "" {
					// The A-side reports per-image inference failures
					// (e.g. an undecodable upload); surface the reason
					// instead of silently counting the image skipped.
					logx.Warnf("remote tagger: image %d failed: %s", id, r.Err)
					skipped++
				} else if r.Tags == nil {
					// Cancelled or interrupted on the A-side; nothing
					// to store. Mirrors RunWithTaggers' Tags==nil
					// sentinel, so a cancelled job is neither stored
					// nor counted as a failure.
				} else if err := storeResults(ctx, database, id, r.Tags, nil, 0); err != nil {
					logx.Warnf("remote tagger: store results for image %d: %v", id, err)
					skipped++
				}
				done++
				progressed = true
				mgr.Update(done, total, "remote tagging")
			}
		}

		// Drop jobs that never completed in time (A-side crash, network
		// partition); count them skipped so the batch terminates. The
		// A-side is told to drop them too so it stops tagging images
		// nobody will consume.
		now := time.Now()
		var timedOut []string
		for jobID := range outstanding {
			if at, ok := submittedAt[jobID]; ok && now.Sub(at) > remoteJobTimeout {
				timedOut = append(timedOut, jobID)
				logx.Warnf("remote tagger: job %s timed out after %s; skipping image %d", jobID, remoteJobTimeout, outstanding[jobID])
				delete(outstanding, jobID)
				delete(submittedAt, jobID)
				skipped++
				done++
				progressed = true
			}
		}
		if len(timedOut) > 0 {
			notifyRemoteCancel(remote, timedOut)
		}

		// 2) Refill the window up to capacity.
		for len(outstanding) < capacity && len(remaining) > 0 {
			target := remaining[0]
			jobID, err := remote.Submit(ctx, BackendImageRequest{ID: target.id, FramePaths: []string{target.path}})
			if err != nil {
				if errors.Is(err, errRemoteQueueFull) {
					// Queue is full; drain again next iteration.
					break
				}
				var rejected *remoteSubmitRejectedError
				if errors.As(err, &rejected) {
					if rejected.status == http.StatusBadRequest {
						// The A-side rejected this image outright (e.g.
						// an unsupported format); retrying would loop
						// forever on the same file, so count it skipped
						// and move on to the next image.
						logx.Warnf("remote tagger: image %d rejected by A-side: %s", target.id, rejected.body)
						remaining = remaining[1:]
						skipped++
						done++
						progressed = true
						mgr.Update(done, total, "remote tagging")
						continue
					}
					// Authentication or configuration failures (401/403)
					// affect every image; fail the whole job instead of
					// spinning on retries.
					return skipped, fmt.Errorf("remote tagger: submission rejected with %d: %s", rejected.status, rejected.body)
				}
				// Transient failure: back off and retry the same id.
				if ctx.Err() != nil {
					notifyRemoteCancel(remote, keysOf(outstanding))
					return skipped, ctx.Err()
				}
				select {
				case <-time.After(500 * time.Millisecond):
				case <-ctx.Done():
					notifyRemoteCancel(remote, keysOf(outstanding))
					return skipped, ctx.Err()
				}
				break
			}
			remaining = remaining[1:]
			outstanding[jobID] = target.id
			submittedAt[jobID] = time.Now()
			progressed = true
		}

		// 3) Prevent busy-spin: when nothing drained and nothing was
		// accepted (queue full / transient error), wait before the next
		// drain so the A-side has time to produce results.
		if !progressed {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				notifyRemoteCancel(remote, keysOf(outstanding))
				return skipped, ctx.Err()
			}
		}
	}

	mgr.Update(done, total, "remote tagging: done")
	return skipped, nil
}

// notifyRemoteCancel best-effort tells the A-side to drop the given
// jobs so it stops tagging images nobody will consume. Called when the
// local job is cancelled or a submitted job times out; it never blocks
// the caller for long and failures are only logged.
func notifyRemoteCancel(remote *remoteBackend, jobIDs []string) {
	if len(jobIDs) == 0 {
		return
	}
	// The job context may already be cancelled at this point, so the
	// notification gets its own short deadline instead.
	nctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := remote.Cancel(nctx, jobIDs, false); err != nil {
		logx.Warnf("remote tagger: notify A-side cancel for %d job(s): %v", len(jobIDs), err)
	}
}

// keysOf returns the keys of m in arbitrary order.
func keysOf(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readFileHead reads up to 12 bytes from path for content sniffing.
func readFileHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	return head[:n], nil
}

func isStaticImageType(ft string) bool {
	switch ft {
	case "jpeg", "png", "webp", "gif":
		return true
	}
	return false
}
