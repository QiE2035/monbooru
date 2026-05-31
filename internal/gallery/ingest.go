package gallery

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
)

// FolderPath computes the relative directory of filePath under
// galleryPath. Returns "" for files at the gallery root. Linux paths.
func FolderPath(galleryPath, filePath string) string {
	dir := filepath.Dir(filePath)
	if dir == "." {
		return ""
	}
	rel := strings.TrimPrefix(dir, galleryPath)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "." {
		return ""
	}
	return rel
}

// Ingest processes a single file: hash, dimension probe, metadata
// extraction, DB insert, thumbnail. Returns (image, isDuplicate, error).
// origin records how the file got in ("ingest" / "upload" / caller-supplied
// string); empty defaults to "ingest".
func Ingest(database *db.DB, galleryPath, thumbnailsPath, path, fileType, origin string) (*models.Image, bool, error) {
	hash, err := HashFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("hashing file: %w", err)
	}
	ClaimOwnership(path)
	return ingestWithHash(database, galleryPath, thumbnailsPath, path, fileType, hash, origin)
}

// ingestWithHash is the body of Ingest minus the HashFile +
// ClaimOwnership preamble. Sync uses it directly to avoid double-hashing
// the same file on large libraries.
func ingestWithHash(database *db.DB, galleryPath, thumbnailsPath, path, fileType, hash, origin string) (*models.Image, bool, error) {
	if origin == "" {
		origin = models.OriginIngest
	}
	var existingID int64
	err := database.Read.QueryRow(
		`SELECT id FROM images WHERE sha256 = ?`, hash,
	).Scan(&existingID)

	if err == nil {
		var img models.Image
		var isMissingInt int
		scanErr := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt)
		if scanErr != nil {
			return nil, true, fmt.Errorf("looking up duplicate image %d: %w", existingID, scanErr)
		}
		img.IsMissing = isMissingInt == 1

		if img.IsMissing {
			// Previously-missing file has reappeared; reactivate it.
			// Demote the previous canonical to alias and upsert the new
			// path as canonical (mirrors the watcher-mv branch below) so
			// the prior path stays in image_paths for history.
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin reactivation tx: %w", txErr)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(
				`UPDATE images SET is_missing = 0, canonical_path = ?, folder_path = ? WHERE id = ?`,
				path, newFolder, existingID,
			); err != nil {
				return nil, false, fmt.Errorf("reactivate image: %w", err)
			}
			if _, err := tx.Exec(
				`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND is_canonical = 1`,
				existingID,
			); err != nil {
				return nil, false, fmt.Errorf("demote previous canonical: %w", err)
			}
			if _, err := tx.Exec(
				`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)
				 ON CONFLICT(path) DO UPDATE SET is_canonical = 1`,
				existingID, path,
			); err != nil {
				return nil, false, fmt.Errorf("install new canonical: %w", err)
			}
			// Restore the usage_count slots markFileMissing decremented
			// when the file vanished. usage_count tracks visible images,
			// so a reactivated row owes +1 to every tag still attached.
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = usage_count + 1
				 WHERE id IN (SELECT tag_id FROM image_tags WHERE image_id = ?)`,
				existingID,
			); err != nil {
				return nil, false, fmt.Errorf("restore usage counts: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit reactivation: %w", err)
			}
			Generate(path, thumbnailsPath, existingID, img.FileType)
			img.IsMissing = false
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: reactivated previously missing image id=%d path=%q", existingID, path)
			return &img, false, nil
		}

		// Watcher-observed mv inside the gallery: fsnotify emits a Create
		// for the new path while the old canonical_path is already gone
		// from disk. Without promotion the row would stay pinned to the
		// vanished location and the file would disappear from folder
		// filters. Mirrors the alias-promotion branch in Sync.
		if _, statErr := os.Stat(img.CanonicalPath); statErr != nil {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin promote tx: %w", txErr)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(
				`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
				path, newFolder, existingID,
			); err != nil {
				return nil, false, fmt.Errorf("promote canonical path: %w", err)
			}
			if _, err := tx.Exec(
				`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND path = ?`,
				existingID, img.CanonicalPath,
			); err != nil {
				return nil, false, fmt.Errorf("demote old canonical: %w", err)
			}
			if _, err := tx.Exec(
				`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)
				 ON CONFLICT(path) DO UPDATE SET is_canonical = 1`,
				existingID, path,
			); err != nil {
				return nil, false, fmt.Errorf("install new canonical: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit promote: %w", err)
			}
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: promoted alias to canonical for image id=%d (old path gone) %q", existingID, path)
			return &img, false, nil
		}

		// Normal duplicate: record this path as an alias.
		_, aliasErr := database.Write.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		)
		if aliasErr != nil {
			logx.Warnf("ingest alias: %v", aliasErr)
		}
		return &img, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, fmt.Errorf("checking sha256: %w", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("stat file: %w", err)
	}

	folderPath := FolderPath(galleryPath, path)

	var imgWidth, imgHeight *int
	var pageCount *int
	var durationSec *float64
	var prefilledSeries string
	var mangaMeta *models.MangaMetadata
	if IsVideoType(fileType) {
		if d, ok := ProbeDurationSeconds(path); ok {
			durationSec = &d
		}
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
	}
	if fileType == models.FileTypeCBZ {
		archive, openErr := OpenManga(path)
		if openErr != nil {
			// Empty / corrupt cbz: log and skip without inserting a
			// row. The watcher / sync caller treats this as a
			// non-fatal "skip" the same way a video without ffmpeg is
			// treated - the file stays on disk for the operator.
			logx.Warnf("ingest: skip manga %q: %v", path, openErr)
			return nil, false, fmt.Errorf("ingest manga: %w", openErr)
		}
		w, h, dimErr := archive.CoverDimensions()
		if dimErr == nil {
			imgWidth, imgHeight = &w, &h
		} else {
			logx.Warnf("ingest manga %q: cover dimensions: %v", path, dimErr)
		}
		mm, mmErr := metadata.ParseComicInfo(archive.Reader())
		if mmErr != nil {
			logx.Warnf("ingest manga %q: ComicInfo: %v", path, mmErr)
		} else if mm != nil {
			mangaMeta = mm
			prefilledSeries = mm.Series
		}
		pcVal := len(archive.Pages)
		pageCount = &pcVal
		archive.Close()
	} else if !IsVideoType(fileType) {
		f, openErr := os.Open(path)
		if openErr == nil {
			if cfg2, _, decErr := image.DecodeConfig(f); decErr == nil {
				w, h := cfg2.Width, cfg2.Height
				imgWidth, imgHeight = &w, &h
			}
			f.Close()
		}
	}

	var sdMeta *models.SDMetadata
	var comfyMeta *models.ComfyUIMetadata
	if fileType != models.FileTypeCBZ {
		sdMeta, comfyMeta, _ = metadata.Extract(path, fileType)
	}
	sourceType := models.SourceTypeNone
	if sdMeta != nil && comfyMeta != nil {
		sourceType = models.SourceTypeBoth
	} else if sdMeta != nil {
		sourceType = models.SourceTypeA1111
	} else if comfyMeta != nil {
		sourceType = models.SourceTypeComfyUI
	}

	// ON CONFLICT(sha256) DO NOTHING so a concurrent ingest that wrote the
	// same SHA between our read-pool check and this transaction falls into
	// the duplicate branch instead of failing with a UNIQUE constraint.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var imgID int64
	insertErr := tx.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, width, height, file_size, source_type, origin, page_count, series, duration_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		hash, path, folderPath, fileType, toNullInt(imgWidth), toNullInt(imgHeight), fi.Size(), sourceType, origin, toNullInt(pageCount), prefilledSeries, toNullFloat(durationSec),
	).Scan(&imgID)

	if insertErr == sql.ErrNoRows {
		// Lost the race to another concurrent ingest. Record this path as
		// an alias of whichever id now owns the SHA and return a duplicate
		// result so the caller logs "duplicate" instead of an error.
		var existingID int64
		if err := tx.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, hash).Scan(&existingID); err != nil {
			return nil, false, fmt.Errorf("race: fetch existing sha: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		); err != nil {
			return nil, false, fmt.Errorf("race: insert alias path: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("race: commit alias: %w", err)
		}
		// Reload the existing image record so callers see the real state.
		var img models.Image
		var isMissingInt int
		if err := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt); err != nil {
			return nil, true, fmt.Errorf("race: reload existing image %d: %w", existingID, err)
		}
		img.IsMissing = isMissingInt == 1
		return &img, true, nil
	}
	if insertErr != nil {
		return nil, false, fmt.Errorf("inserting image: %w", insertErr)
	}

	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix) VALUES (?, ?, 1, ?)`,
		imgID, path, fi.ModTime().Unix(),
	); err != nil {
		return nil, false, fmt.Errorf("inserting image_path: %w", err)
	}

	if sdMeta != nil {
		sdMeta.ImageID = imgID
		if err := insertSDMeta(tx, sdMeta); err != nil {
			return nil, false, fmt.Errorf("inserting sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imgID
		if err := insertComfyMeta(tx, comfyMeta); err != nil {
			return nil, false, fmt.Errorf("inserting comfyui_metadata: %w", err)
		}
	}
	if mangaMeta != nil {
		mangaMeta.ImageID = imgID
		if err := insertMangaMeta(tx, mangaMeta); err != nil {
			return nil, false, fmt.Errorf("inserting manga_metadata: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing ingest: %w", err)
	}

	if err := Generate(path, thumbnailsPath, imgID, fileType); err != nil {
		logx.Warnf("thumbnail generation failed for %q: %v", path, err)
	} else if err := RecomputeAndStorePhash(context.Background(), database, imgID, thumbnailsPath); err != nil {
		// Phash failures on a successful thumbnail are rare (decode
		// shouldn't fail on a JPEG we just wrote). Log and continue -
		// images.phash stays NULL and the row is invisible to the
		// relations system until a future recompute lands.
		logx.Warnf("phash compute failed for %q: %v", path, err)
	}

	img := &models.Image{
		ID:            imgID,
		SHA256:        hash,
		CanonicalPath: path,
		FolderPath:    folderPath,
		FileType:      fileType,
		Width:         imgWidth,
		Height:        imgHeight,
		FileSize:      fi.Size(),
		SourceType:    sourceType,
		Origin:        origin,
		PageCount:     pageCount,
		DurationSec:   durationSec,
		Series:        prefilledSeries,
		IngestedAt:    time.Now().UTC(),
	}
	return img, false, nil
}

func toNullInt(v *int) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func toNullFloat(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func insertSDMeta(tx *sql.Tx, sd *models.SDMetadata) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sd.ImageID, sd.Prompt, sd.NegativePrompt, sd.Model, sd.Seed, sd.Sampler, sd.Steps, sd.CFGScale, sd.RawParams, sd.GenerationHash,
	)
	return err
}

func insertComfyMeta(tx *sql.Tx, comfy *models.ComfyUIMetadata) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		comfy.ImageID, comfy.Prompt, comfy.ModelCheckpoint, comfy.Seed, comfy.Sampler, comfy.Steps, comfy.CFGScale, comfy.RawWorkflow, comfy.GenerationHash,
	)
	return err
}

func insertMangaMeta(tx *sql.Tx, m *models.MangaMetadata) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO manga_metadata (image_id, title, series, number, volume, count, summary, notes,
		     year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		     imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ImageID, m.Title, m.Series, m.Number, m.Volume,
		toNullInt(m.Count), m.Summary, m.Notes,
		toNullInt(m.Year), toNullInt(m.Month), toNullInt(m.Day),
		m.Writer, m.Penciller, m.Inker, m.Colorist, m.Letterer, m.CoverArtist, m.Editor, m.Publisher,
		m.Imprint, m.Genre, m.Web, m.LanguageISO, m.Format, m.Manga, m.AgeRating,
		toNullFloat(m.CommunityRating), toNullInt(m.XMLPageCount), m.RawXML,
	)
	return err
}
