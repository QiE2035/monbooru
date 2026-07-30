package tagger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
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

func runRemoteTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	remote := newRemoteBackend(cfg.Tagger.RemoteClient.URL, cfg.Tagger.RemoteClient.Token)

	// Batch size scales with the configured parallel workers so the A-side
	// never receives more work per request than it can keep up with.
	batchSize := max(1, cfg.Tagger.Parallel)
	total := len(ids)
	done := 0
	var skipped int
	batchNum := 0
	batchCount := (total + batchSize - 1) / batchSize

	for start := 0; start < total; start += batchSize {
		if ctx.Err() != nil {
			return skipped, ctx.Err()
		}
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := ids[start:end]
		batchNum++

		mgr.Update(done, total, fmt.Sprintf("remote tagging: batch %d/%d", batchNum, batchCount))

		requests := make([]BackendImageRequest, 0, len(batch))
		for _, id := range batch {
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
			data, err := os.ReadFile(canonPath)
			if err != nil {
				skipped++
				done++
				continue
			}
			requests = append(requests, BackendImageRequest{
				ID: id, FrameBytes: [][]byte{data},
			})
		}

		if len(requests) == 0 {
			continue
		}

		resp, err := remote.Run(ctx, RunRequest{Images: requests})
		if err != nil {
			return skipped + done, fmt.Errorf("batch around image %d: %w", start, err)
		}

		for _, r := range resp.Results {
			if r.Err != "" {
				skipped++
			} else {
				if err := storeResults(ctx, database, r.ID, r.Tags, nil, 0); err != nil {
					skipped++
				}
			}
			done++
		}
	}

	mgr.Update(done, total, "remote tagging: done")
	return skipped, nil
}

func isStaticImageType(ft string) bool {
	switch ft {
	case "jpeg", "png", "webp", "gif":
		return true
	}
	return false
}
