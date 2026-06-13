package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"fmt"
	"math/bits"
	"strings"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite"
)

// basenameSQL is the body of the SQLite `basename(path)` scalar
// function. Returns the substring of `path` after the last `/`; if
// `path` has no `/`, returns it unchanged. NULL passes through as
// NULL. Used by the search executor's `name:` filter so the match
// can target the filename segment without bleeding into folder
// names; pure SQLite has no built-in for this since `reverse()`
// isn't part of the modernc build, so registering the function is
// the cleanest path that avoids a denormalised `basename` column.
func basenameSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 || args[0] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, nil
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:], nil
	}
	return s, nil
}

// hammingDistSQL is the body of the SQLite `hammingdist(int64, int64)`
// scalar function: Hamming distance between the two 64-bit values
// interpreted as unsigned bit patterns. SQLite has no XOR operator on
// integers (^ is unsupported in the modernc dialect), so the search
// executor calls this when the per-gallery BK-tree isn't wired and
// it needs to compute distance in pure SQL for the `phash:<hex>~d`
// filter. NULL on either side passes through as NULL so a phashless
// row drops out of the comparison.
func hammingDistSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	a, aOk := args[0].(int64)
	b, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(bits.OnesCount64(uint64(a) ^ uint64(b))), nil
}

// randomKeySQL is the body of the SQLite `random_key(image_id, seed)`
// scalar function. Maps (id, seed) to a deterministic 63-bit value via
// a SplitMix64-style mix so the executor's random-sort ORDER BY
// produces a uniformly scattered permutation even for small seeds.
//
// NULL on either side falls through as NULL so SQLite's NULL-ordering
// rules apply consistently with the (id, key) cursor comparison.
func randomKeySQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	id, aOk := args[0].(int64)
	seed, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(RandomSortKey(id, seed)), nil
}

// RandomSortKey computes the same 63-bit key SQLite's random_key()
// emits, so Go-side callers (cursor seek in ExecuteAdjacent, rank
// computation in RankInQuery) can produce the matching keyVal without
// a round trip. Stable across calls; depends only on (id, seed).
func RandomSortKey(id, seed int64) uint64 {
	x := uint64(id)*0x9E3779B97F4A7C15 ^ uint64(seed)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x & 0x7FFFFFFFFFFFFFFF
}

func init() {
	// Registered once for the driver; available on every connection
	// opened afterwards.
	sqlite.MustRegisterDeterministicScalarFunction("basename", 1, basenameSQL)
	sqlite.MustRegisterDeterministicScalarFunction("hammingdist", 2, hammingDistSQL)
	sqlite.MustRegisterDeterministicScalarFunction("random_key", 2, randomKeySQL)
}

//go:embed schema.sql
var schemaSQL string

// bootstrapSchemaVersion is the marker Bootstrap stores in
// PRAGMA user_version once it has applied every migration in this file
// and refreshed sqlite_stat1. Bump it when a migration adds a column or
// index the planner needs stats for; Bootstrap then runs ANALYZE on the
// next boot after the upgrade and skips it on every boot afterwards.
const bootstrapSchemaVersion = 8

// DB holds read and write connection pools for the SQLite database.
// WAL mode allows concurrent readers but serialises writers, so the read
// pool has many connections and the write pool has one.
type DB struct {
	Read  *sql.DB
	Write *sql.DB

	// untaggedVisible / autoUntaggedVisible cache the count subtrahends
	// behind tagged:true / autotagged:true partition reads. The
	// underlying NOT EXISTS walk over image_tags is multi-second on a
	// million-row library; InvalidateCachedCounts drops both on every
	// image_tags membership write.
	untaggedVisible     atomic.Pointer[int]
	autoUntaggedVisible atomic.Pointer[int]
}

// Open opens both connection pools pointing at the same SQLite file.
func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-1024)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(67108864)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)
	rd.SetConnMaxIdleTime(5 * time.Minute)

	wr, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)
	wr.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{Read: rd, Write: wr}

	if err := rd.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// Bootstrap runs the embedded schema.sql on the write pool, then applies
// idempotent column-add migrations for databases that predate a column.
// SQLite has no ADD COLUMN IF NOT EXISTS, so each migration gates itself
// on pragma_table_info.
func Bootstrap(db *DB) error {
	b := &bootstrapper{db: db}
	b.exec("bootstrapping schema", schemaSQL)
	b.ensureColumn("images", "origin", `ALTER TABLE images ADD COLUMN origin TEXT NOT NULL DEFAULT 'ingest'`)
	b.ensureColumn("image_tags", "is_implied", `ALTER TABLE image_tags ADD COLUMN is_implied INTEGER NOT NULL DEFAULT 0`)
	// is_inbox: pre-feature libraries upgrade as fully curated. The column
	// default is 1 (new ingests land in the inbox), but existing rows added
	// before the column existed would all flip to "needs triage" without
	// this one-shot - which would dump the operator's whole library into
	// the inbox view on first boot. The pre-count gate in
	// backfillIfFreshColumn detects the just-added case so the UPDATE
	// only runs then.
	b.backfillIfFreshColumn("images", "is_inbox",
		`ALTER TABLE images ADD COLUMN is_inbox INTEGER NOT NULL DEFAULT 1`,
		`UPDATE images SET is_inbox = 0`,
		"backfill is_inbox=0 on upgrade")
	// Partial seek index for the inbox-count cache and the inbox: filter's
	// fastCountInbox path. Created here rather than in schema.sql because
	// the column is added by ensureColumn above on existing libraries; an
	// index in schema.sql would run before the ALTER and reference a
	// missing column.
	b.exec("create idx_images_inbox_visible", `CREATE INDEX IF NOT EXISTS idx_images_inbox_visible ON images(is_inbox) WHERE is_missing = 0`)
	// idx_image_tags_tag(tag_id) is superseded by
	// idx_image_tags_tag_image(tag_id, image_id) - same leading column,
	// same seek selectivity, plus image_id is now covering. Drop on
	// upgrade so existing libraries don't pay disk and write overhead
	// on a redundant index.
	b.exec("drop superseded idx_image_tags_tag", `DROP INDEX IF EXISTS idx_image_tags_tag`)
	b.ensureColumn("images", "source", `ALTER TABLE images ADD COLUMN source TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("images", "url", `ALTER TABLE images ADD COLUMN url TEXT NOT NULL DEFAULT ''`)
	// The historical idx_images_source pointed at images(source_type); the
	// name now belongs to the new images(source) column. Drop the old
	// shape unconditionally and let schema.sql / the recreate below
	// rebuild it under both names.
	b.exec("drop legacy idx_images_source", `DROP INDEX IF EXISTS idx_images_source`)
	b.exec("create idx_images_source_type", `CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type)`)
	b.exec("create idx_images_source", `CREATE INDEX IF NOT EXISTS idx_images_source ON images(source)`)
	// Partial visible source index for the source: filter. The NOCASE-
	// collated variant is what the source: filter equality
	// (`source = ? COLLATE NOCASE`) seeks against; the BINARY-collated
	// index stays for the source autocomplete's prefix-range query that
	// needs binary ordering. Both partials are gated only on
	// `is_missing = 0` so SQLite's planner can match the partial WHERE
	// against any source: filter query (a `source != ''` clause in the
	// partial would force the query to also include it, which the
	// executor doesn't emit).
	b.exec("create idx_images_source_visible", `CREATE INDEX IF NOT EXISTS idx_images_source_visible ON images(source) WHERE is_missing = 0`)
	b.exec("create idx_images_source_nocase_visible", `CREATE INDEX IF NOT EXISTS idx_images_source_nocase_visible ON images(source COLLATE NOCASE) WHERE is_missing = 0`)
	b.ensureColumn("images", "page_count", `ALTER TABLE images ADD COLUMN page_count INTEGER`)
	b.ensureColumn("images", "duration_seconds", `ALTER TABLE images ADD COLUMN duration_seconds REAL`)
	// Partial visible duration index for the duration: filter. Excludes
	// NULL so non-video rows don't carry an entry.
	b.exec("create idx_images_duration_visible", `CREATE INDEX IF NOT EXISTS idx_images_duration_visible ON images(duration_seconds) WHERE is_missing = 0 AND duration_seconds IS NOT NULL`)
	// VIRTUAL generated column over the lowercased filename basename so
	// the name: filter and the system:name autocomplete seek a single
	// indexed string instead of running lower(basename(canonical_path))
	// per row of a full canonical_path scan. STORED isn't reachable via
	// ALTER TABLE; VIRTUAL keeps the value computed on read but lets the
	// matching index materialise it once per row at index-write time, so
	// the seek is the same cost as a real column.
	b.ensureColumn("images", "basename_lower",
		`ALTER TABLE images ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(canonical_path))) VIRTUAL`)
	b.exec("create idx_images_basename_lower_visible", `CREATE INDEX IF NOT EXISTS idx_images_basename_lower_visible ON images(basename_lower) WHERE is_missing = 0 AND basename_lower != ''`)
	// Mirror of images.basename_lower for alias paths so the name:
	// filter's EXISTS over image_paths reads `ip.basename_lower`
	// directly instead of running lower(basename(ip.path)) per alias
	// row. VIRTUAL so the value is computed on read; the EXISTS
	// subquery rides idx_image_paths_aliases (image_id WHERE
	// is_canonical = 0) to skip every canonical row, which is the
	// other half of the per-row cost.
	b.ensureColumn("image_paths", "basename_lower",
		`ALTER TABLE image_paths ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(path))) VIRTUAL`)
	b.ensureColumn("images", "series", `ALTER TABLE images ADD COLUMN series TEXT NOT NULL DEFAULT ''`)
	// Operator-edited per-image position within its series. NULL means
	// "no specific order" - the search executor sorts those after rows
	// with a numeric position when a series: filter pins the result set.
	b.ensureColumn("images", "series_order", `ALTER TABLE images ADD COLUMN series_order INTEGER`)
	b.exec("create idx_images_series", `CREATE INDEX IF NOT EXISTS idx_images_series ON images(series) WHERE series != ''`)
	// NOCASE-collated companion for the collection: filter equality
	// (`series = ? COLLATE NOCASE`); the BINARY index above stays for
	// the collection-autocomplete prefix-range query that needs binary
	// ordering. The NOCASE partial is gated only on the visibility
	// filter the executor emits (no `series != ''` clause), so SQLite
	// can match the partial WHERE against any collection: query.
	b.exec("create idx_images_series_nocase", `CREATE INDEX IF NOT EXISTS idx_images_series_nocase ON images(series COLLATE NOCASE)`)
	// NOCASE-collated companion for folder: equality - same shape as
	// idx_images_folder_visible (already partial WHERE is_missing = 0)
	// but with the COLLATE NOCASE that the folder: filter uses, so the
	// equality leg of the (path = ? COLLATE NOCASE OR path LIKE ...)
	// composite predicate can ride an indexed seek instead of falling
	// back to idx_images_missing + TEMP B-TREE FOR ORDER BY.
	b.exec("create idx_images_folder_nocase_visible", `CREATE INDEX IF NOT EXISTS idx_images_folder_nocase_visible ON images(folder_path COLLATE NOCASE) WHERE is_missing = 0`)
	// Saved-search reproduces the URL the operator was looking at; the
	// seed bit lets a `random` save reopen at the same shuffle. `sort_order`
	// is the URL's `order` value - column name is suffixed because `order`
	// is a SQLite reserved word that breaks plain UPDATE/INSERT statements
	// even with quoting in some driver paths.
	b.ensureColumn("saved_searches", "sort", `ALTER TABLE saved_searches ADD COLUMN sort TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("saved_searches", "sort_order", `ALTER TABLE saved_searches ADD COLUMN sort_order TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("saved_searches", "seed", `ALTER TABLE saved_searches ADD COLUMN seed TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_paths", "mtime_unix", `ALTER TABLE image_paths ADD COLUMN mtime_unix INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("images", "phash", `ALTER TABLE images ADD COLUMN phash INTEGER`)
	// Per-upload batch token stamped on web-UI uploads; NULL elsewhere. No
	// index - it is only read for the page of rows the inbox cluster view
	// already loaded, never filtered or sorted on.
	b.ensureColumn("images", "upload_batch", `ALTER TABLE images ADD COLUMN upload_batch INTEGER`)
	// Partial phash index: drives `phash:<hex>` exact-match seeks and
	// the cold-path SELECT that loads the BK-tree at first relations
	// query. Skips NULL rows (the BK-tree only carries computed phashes)
	// so a half-backfilled library doesn't pay the storage for unhashed
	// entries.
	b.exec("create idx_images_phash", `CREATE INDEX IF NOT EXISTS idx_images_phash ON images(phash) WHERE phash IS NOT NULL`)
	// Stored tag_count column maintained by triggers on image_tags so
	// the tagcount: filter rides an indexed range seek instead of a
	// correlated `SELECT COUNT(*) FROM image_tags WHERE image_id = i.id`
	// per visible row. Backfilled once on first boot after the column
	// is added; the triggers below keep it in lockstep with every
	// image_tags insert/delete.
	b.backfillIfFreshColumn("images", "tag_count",
		`ALTER TABLE images ADD COLUMN tag_count INTEGER NOT NULL DEFAULT 0`,
		`UPDATE images SET tag_count = (SELECT COUNT(*) FROM image_tags WHERE image_id = images.id)`,
		"backfill images.tag_count")
	b.exec("create idx_images_tag_count_visible", `CREATE INDEX IF NOT EXISTS idx_images_tag_count_visible ON images(tag_count) WHERE is_missing = 0`)
	// Partial covering index for `mime:` / `type:` so the planner can
	// seek `i.file_type IN (...)` instead of scanning every visible row.
	// The bucket is small (jpeg, png, webp, gif, mp4, webm, cbz) so the
	// index stays compact even at large scale.
	b.exec("create idx_images_file_type_visible", `CREATE INDEX IF NOT EXISTS idx_images_file_type_visible ON images(file_type) WHERE is_missing = 0`)
	// Maintain images.tag_count with row-level triggers. MAX(0, ...) on
	// the delete trigger guards against the impossible-but-cheap case of
	// a negative count from a torn upgrade. The triggers are FOR EACH
	// ROW (SQLite's only mode) so a batch INSERT/DELETE on image_tags
	// fires one UPDATE per affected image; the per-row cost is a primary-
	// key seek on images plus an indexed update.
	b.exec("create trg_image_tags_count_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ai
		AFTER INSERT ON image_tags
		BEGIN
			UPDATE images SET tag_count = tag_count + 1 WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_count_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ad
		AFTER DELETE ON image_tags
		BEGIN
			UPDATE images SET tag_count = MAX(0, tag_count - 1) WHERE id = OLD.image_id;
		END`)
	// Stored rating_rank column maintained by triggers on image_tags. The
	// SFW cookie ceiling AND-chains three NOT EXISTS subqueries (one per
	// excluded rating tag) onto every search expression, which the deep-
	// gallery cursor walks per visible row; reading a single integer
	// column with a covering partial index collapses that chain to one
	// indexed range. -1 sentinel for "no rating tag" lets the ceiling
	// predicate stay `rating_rank <= ?` without an OR-NULL clause:
	// unrated rows pass every ceiling because -1 is below every
	// documented level (general=0, sensitive=1, questionable=2,
	// explicit=3). PruneLowerRatingsTx and the autotagger uphold
	// at-most-one-rating-per-image, so the column carries the single
	// rank that survived; pre-existing rows with multiple ratings get
	// the MAX during backfill, matching the search engine's "highest-
	// wins" semantics.
	b.backfillIfFreshColumn("images", "rating_rank",
		`ALTER TABLE images ADD COLUMN rating_rank INTEGER NOT NULL DEFAULT -1`,
		`UPDATE images SET rating_rank = COALESCE((
			SELECT MAX(CASE t.name
				WHEN 'general' THEN 0
				WHEN 'sensitive' THEN 1
				WHEN 'questionable' THEN 2
				WHEN 'explicit' THEN 3
				ELSE -1 END)
			FROM image_tags it
			JOIN tags t ON t.id = it.tag_id
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = images.id AND tc.name = 'rating'
		), -1)`,
		"backfill images.rating_rank")
	// Partial covering index for `fav:true` searches. The bare
	// idx_images_favorited (CREATE in schema.sql) covers both polarities
	// but isn't partial; the planner under c=5 sometimes prefers
	// idx_images_missing and pays a TEMP B-TREE for the cursor sort.
	// The favorited subset is small (low single-percent on a typical
	// library) so the composite (ingested_at, id) tail lets the data
	// SELECT walk favorited matches in sort order with no temp sort.
	b.exec("create idx_images_favorited_visible", `CREATE INDEX IF NOT EXISTS idx_images_favorited_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0 AND is_favorited = 1`)
	// Covering partial sort indexes that include rating_rank as a tail
	// key. The ceiling chain rewrite emits `i.rating_rank <= ?` as the
	// only non-cursor predicate; with rating_rank inlined in the index
	// the deep-gallery cursor walks (ingested_at, id) in order and
	// filters rating_rank without dropping back to the table. The
	// matching filesize-sort companion covers `back_sort=filesize` on
	// the same ceiling path. Both are partial WHERE is_missing = 0 so
	// the entries match the gallery's visible-only filter.
	b.exec("create idx_images_ingested_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_ingested_rating_visible ON images(ingested_at DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	b.exec("create idx_images_filesize_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_filesize_rating_visible ON images(file_size DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	// Standalone covering partial index on the effective rating rank so
	// the unfiltered-ceiling count rides a `rating_rank <= ?` range seek
	// instead of scanning every visible row. The sort indexes above carry
	// rating_rank only as a trailing key, which the planner can't seek on
	// for a bare rank predicate.
	b.exec("create idx_images_rating_rank_visible", `CREATE INDEX IF NOT EXISTS idx_images_rating_rank_visible ON images(rating_rank) WHERE is_missing = 0`)
	// Maintain images.rating_rank with row-level triggers. The WHEN
	// subquery short-circuits the trigger to rating-category writes only
	// so per-image_tags inserts and deletes for non-rating tags stay free.
	// Each fire recomputes MAX over the image's remaining rating tags;
	// on the insert side that includes the just-added row, on the delete
	// side it excludes the just-deleted row, mirroring the highest-wins
	// invariant PruneLowerRatingsTx enforces at write time.
	b.exec("create trg_image_tags_rating_rank_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ai
		AFTER INSERT ON image_tags
		WHEN NEW.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = NEW.image_id AND tc.name = 'rating'
			), -1) WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_rating_rank_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ad
		AFTER DELETE ON image_tags
		WHEN OLD.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = OLD.image_id AND tc.name = 'rating'
			), -1) WHERE id = OLD.image_id;
		END`)
	// FTS5 trigram virtual tables for the name: filter. The outer leg's
	// `i.basename_lower LIKE '%val%'` cannot ride the existing
	// idx_images_basename_lower_visible index because of the leading
	// wildcard, so the planner walks every visible row. With the
	// trigram tokenizer the LIKE-substring search becomes an index
	// lookup (rowid = the canonical or alias path id). Queries shorter
	// than 3 characters still fall back to the LIKE shape because the
	// trigram tokenizer needs at least 3 characters of overlap to seek.
	b.exec("create image_basename_canonical_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_canonical_fts USING fts5(basename, tokenize='trigram', content='', contentless_delete=1)`)
	b.exec("create image_basename_alias_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_alias_fts USING fts5(basename, image_id UNINDEXED, tokenize='trigram', content='', contentless_delete=1)`)
	// Backfill on the same version-gate as ANALYZE so a partial backfill
	// from a torn upgrade gets retried until the marker advances. The
	// trigram FTS is contentless so re-inserting a (rowid, basename)
	// pair after a DELETE FROM stays cheap; clear stale entries first.
	// Resolve b.err before reading user_version so a failed earlier
	// exec doesn't query against a half-bootstrapped DB.
	if b.err != nil {
		return b.err
	}
	var ratingRankUserVersion int
	if err := db.Write.QueryRow(`PRAGMA user_version`).Scan(&ratingRankUserVersion); err != nil {
		return fmt.Errorf("read user_version (fts5 backfill): %w", err)
	}
	if ratingRankUserVersion < bootstrapSchemaVersion {
		b.exec("clear image_basename_canonical_fts", `DELETE FROM image_basename_canonical_fts`)
		b.exec("backfill image_basename_canonical_fts", `INSERT INTO image_basename_canonical_fts (rowid, basename)
			SELECT id, basename_lower FROM images WHERE basename_lower != ''`)
		b.exec("clear image_basename_alias_fts", `DELETE FROM image_basename_alias_fts`)
		b.exec("backfill image_basename_alias_fts", `INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
			SELECT id, basename_lower, image_id FROM image_paths WHERE is_canonical = 0 AND basename_lower != ''`)
	}
	// Triggers maintain the FTS5 tables in lockstep with the source rows.
	// canonical_path's basename_lower is a VIRTUAL generated column;
	// SQLite resolves NEW.basename_lower / OLD.basename_lower per trigger
	// fire. The rowid alignment (canonical = images.id, alias =
	// image_paths.id) keeps every UPDATE / DELETE O(1) on the FTS rowid.
	// `INSERT OR REPLACE` collapses the (rowid) primary-key conflict on
	// ALTER-style updates that don't change the column but do retrigger
	// the OF clause (rare; defensive).
	b.exec("create trg_image_basename_canonical_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ai
		AFTER INSERT ON images
		WHEN NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_canonical_fts (rowid, basename) VALUES (NEW.id, NEW.basename_lower);
		END`)
	b.exec("create trg_image_basename_canonical_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_au
		AFTER UPDATE OF canonical_path ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_canonical_fts (rowid, basename) SELECT NEW.id, NEW.basename_lower WHERE NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_canonical_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ad
		AFTER DELETE ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
		END`)
	b.exec("create trg_image_basename_alias_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ai
		AFTER INSERT ON image_paths
		WHEN NEW.is_canonical = 0 AND NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_alias_fts (rowid, basename, image_id) VALUES (NEW.id, NEW.basename_lower, NEW.image_id);
		END`)
	b.exec("create trg_image_basename_alias_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_au
		AFTER UPDATE OF path, is_canonical ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
				SELECT NEW.id, NEW.basename_lower, NEW.image_id
				WHERE NEW.is_canonical = 0 AND NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_alias_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ad
		AFTER DELETE ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
		END`)
	// ANALYZE only when the schema marker says this version's migrations
	// haven't been analyzed yet. PRAGMA optimize then handles row-count
	// drift on steady-state restarts. Without the gate, ANALYZE on the
	// large image_tags indexes costs ~30 s on a cold OS page cache every
	// boot and blows the coldstart budget; the new partial indexes that
	// pragma optimize misses (idx_images_inbox_visible / idx_images_source
	// / idx_images_series and the rebuilt idx_image_tags_tag_image)
	// instead get their sqlite_stat1 entries once, on the upgrade boot.
	// analysis_limit=400 is the SQLite-recommended sample cap; the
	// resulting stats are accurate enough for plan choice and keep the
	// one-time pass under a second when the DB is warm.
	if b.err != nil {
		return b.err
	}
	var userVersion int
	if err := db.Write.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if userVersion < bootstrapSchemaVersion {
		b.exec("set analysis_limit", `PRAGMA analysis_limit = 400`)
		b.exec("analyze images", `ANALYZE images`)
		b.exec("analyze image_tags", `ANALYZE image_tags`)
		b.exec("set user_version", fmt.Sprintf(`PRAGMA user_version = %d`, bootstrapSchemaVersion))
	}
	b.exec("pragma optimize", `PRAGMA optimize`)
	return b.err
}

func exec(db *DB, label, sql string) error {
	if _, err := db.Write.Exec(sql); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// bootstrapper threads the first migration error through a long
// sequence of calls so each migration step lands as one statement.
// Same shape as jsonWriter in internal/web/gallery_io.go.
type bootstrapper struct {
	db  *DB
	err error
}

func (b *bootstrapper) exec(label, sql string) {
	if b.err != nil {
		return
	}
	b.err = exec(b.db, label, sql)
}

func (b *bootstrapper) ensureColumn(table, column, alterSQL string) {
	if b.err != nil {
		return
	}
	b.err = ensureColumn(b.db, table, column, alterSQL)
}

func (b *bootstrapper) backfillIfFreshColumn(table, column, alterSQL, backfillSQL, backfillLabel string) {
	if b.err != nil {
		return
	}
	b.err = backfillIfFreshColumn(b.db, table, column, alterSQL, backfillSQL, backfillLabel)
}

// backfillIfFreshColumn runs backfillSQL only when the ALTER actually
// added the column - re-bootstraps of an in-use library must not
// overwrite values the triggers have since maintained.
func backfillIfFreshColumn(db *DB, table, column, alterSQL, backfillSQL, backfillLabel string) error {
	var pre int
	if err := db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&pre); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if err := ensureColumn(db, table, column, alterSQL); err != nil {
		return err
	}
	if pre > 0 {
		return nil
	}
	if _, err := db.Write.Exec(backfillSQL); err != nil {
		return fmt.Errorf("%s: %w", backfillLabel, err)
	}
	return nil
}

// ensureColumn adds a column on the named table when it is absent. The
// caller supplies the full ALTER TABLE so the default and type stay
// adjacent to the original schema definition. table_xinfo (vs table_info)
// reports VIRTUAL / STORED generated columns too, so the idempotency
// check survives those.
func ensureColumn(db *DB, table, column, alterSQL string) error {
	var count int
	if err := db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo(?) WHERE name = ?`, table, column,
	).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := db.Write.Exec(alterSQL); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// cachedCount returns the cached value or runs sql once. Errors return
// (0, false) so the fastCount* callers can fall back to the slow path
// without per-call error handling.
func (db *DB) cachedCount(cache *atomic.Pointer[int], sql string) (int, bool) {
	if p := cache.Load(); p != nil {
		return *p, true
	}
	var n int
	if err := db.Read.QueryRow(sql).Scan(&n); err != nil {
		return 0, false
	}
	cache.Store(&n)
	return n, true
}

// UntaggedVisibleCount returns the cached count of visible images that
// carry no image_tags row, or queries it on demand. fastCountTagged
// subtracts this from the visible total to derive an exact tagged:true
// partition without re-walking image_tags on every search.
func (db *DB) UntaggedVisibleCount() (int, bool) {
	return db.cachedCount(&db.untaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)`)
}

// AutoUntaggedVisibleCount is UntaggedVisibleCount restricted to
// image_tags rows carrying is_auto = 1 - the subtrahend behind
// autotagged:true. There is no covering (image_id, is_auto) index, so
// the NOT-EXISTS walk is heavier than the bare-untagged one above.
func (db *DB) AutoUntaggedVisibleCount() (int, bool) {
	return db.cachedCount(&db.autoUntaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM image_tags it
		         WHERE it.image_id = i.id AND it.is_auto = 1
		       )`)
}

// InvalidateCachedCounts drops every per-DB count cache. Call after a
// write that changes image_tags membership (tag add/remove, batch tag,
// implication propagation, autotag ingest, image delete) so the next
// reader recomputes the slow subtrahends from current state. Cheap to
// call - just two atomic stores - so over-invalidating costs nothing.
func (db *DB) InvalidateCachedCounts() {
	db.untaggedVisible.Store(nil)
	db.autoUntaggedVisible.Store(nil)
}

// ShrinkMemory runs `PRAGMA shrink_memory` on every connection in
// both pools, returning freed pages from modernc/sqlite's caches to
// the kernel. Each connection has its own page cache, so all are
// reserved up front to guarantee coverage.
func (db *DB) ShrinkMemory(ctx context.Context) error {
	if err := shrinkPool(ctx, db.Read); err != nil {
		return err
	}
	return shrinkPool(ctx, db.Write)
}

func shrinkPool(ctx context.Context, pool *sql.DB) error {
	n := pool.Stats().MaxOpenConnections
	if n <= 0 {
		n = 1
	}
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if _, err := c.ExecContext(ctx, `PRAGMA shrink_memory`); err != nil {
			return err
		}
	}
	return nil
}

// Close closes both connection pools.
func (db *DB) Close() error {
	var firstErr error
	if err := db.Read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := db.Write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
