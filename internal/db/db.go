package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// bootstrapSchemaVersion is the marker Bootstrap stores in
// PRAGMA user_version once it has applied every migration in this file
// and refreshed sqlite_stat1. Bump it when a migration adds a column or
// index the planner needs stats for; Bootstrap then runs ANALYZE on the
// next boot after the upgrade and skips it on every boot afterwards.
const bootstrapSchemaVersion = 1

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
		"&_pragma=cache_size(-2048)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(268435456)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)
	rd.SetConnMaxIdleTime(5 * time.Minute)

	wr, err := sql.Open("sqlite", dsn)
	if err != nil {
		rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)
	wr.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{Read: rd, Write: wr}

	if err := rd.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// Bootstrap runs the embedded schema.sql on the write pool, then applies
// idempotent column-add migrations for databases that predate a column.
// SQLite has no ADD COLUMN IF NOT EXISTS, so each migration gates itself
// on pragma_table_info.
func Bootstrap(db *DB) error {
	if _, err := db.Write.Exec(schemaSQL); err != nil {
		return fmt.Errorf("bootstrapping schema: %w", err)
	}
	if err := ensureColumn(db, "images", "origin", `ALTER TABLE images ADD COLUMN origin TEXT NOT NULL DEFAULT 'ingest'`); err != nil {
		return err
	}
	if err := ensureColumn(db, "image_tags", "is_implied", `ALTER TABLE image_tags ADD COLUMN is_implied INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// is_inbox: pre-feature libraries upgrade as fully curated. The column
	// default is 1 (new ingests land in the inbox), but existing rows added
	// before the column existed would all flip to "needs triage" without
	// this one-shot - which would dump the operator's whole library into
	// the inbox view on first boot. Detect the just-added case by counting
	// the column before the ALTER and only run the UPDATE then.
	var inboxPre int
	if err := db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('images') WHERE name = 'is_inbox'`,
	).Scan(&inboxPre); err != nil {
		return fmt.Errorf("inspect images.is_inbox: %w", err)
	}
	if err := ensureColumn(db, "images", "is_inbox", `ALTER TABLE images ADD COLUMN is_inbox INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if inboxPre == 0 {
		if _, err := db.Write.Exec(`UPDATE images SET is_inbox = 0`); err != nil {
			return fmt.Errorf("backfill is_inbox=0 on upgrade: %w", err)
		}
	}
	// Partial seek index for the inbox-count cache and the inbox: filter's
	// fastCountInbox path. Created here rather than in schema.sql because
	// the column is added by ensureColumn above on existing libraries; an
	// index in schema.sql would run before the ALTER and reference a
	// missing column.
	if _, err := db.Write.Exec(`CREATE INDEX IF NOT EXISTS idx_images_inbox_visible ON images(is_inbox) WHERE is_missing = 0`); err != nil {
		return fmt.Errorf("create idx_images_inbox_visible: %w", err)
	}
	// idx_image_tags_tag(tag_id) is superseded by
	// idx_image_tags_tag_image(tag_id, image_id) - same leading column,
	// same seek selectivity, plus image_id is now covering. Drop on
	// upgrade so existing libraries don't pay disk and write overhead
	// on a redundant index.
	if _, err := db.Write.Exec(`DROP INDEX IF EXISTS idx_image_tags_tag`); err != nil {
		return fmt.Errorf("drop superseded idx_image_tags_tag: %w", err)
	}
	if err := ensureColumn(db, "images", "source", `ALTER TABLE images ADD COLUMN source TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "images", "url", `ALTER TABLE images ADD COLUMN url TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// The historical idx_images_source pointed at images(source_type); the
	// name now belongs to the new images(source) column. Drop the old
	// shape unconditionally and let schema.sql / the recreate below
	// rebuild it under both names.
	if _, err := db.Write.Exec(`DROP INDEX IF EXISTS idx_images_source`); err != nil {
		return fmt.Errorf("drop legacy idx_images_source: %w", err)
	}
	if _, err := db.Write.Exec(`CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type)`); err != nil {
		return fmt.Errorf("create idx_images_source_type: %w", err)
	}
	if _, err := db.Write.Exec(`CREATE INDEX IF NOT EXISTS idx_images_source ON images(source)`); err != nil {
		return fmt.Errorf("create idx_images_source: %w", err)
	}
	if err := ensureColumn(db, "images", "page_count", `ALTER TABLE images ADD COLUMN page_count INTEGER`); err != nil {
		return err
	}
	if err := ensureColumn(db, "images", "series", `ALTER TABLE images ADD COLUMN series TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// Operator-edited per-image position within its series. NULL means
	// "no specific order" - the search executor sorts those after rows
	// with a numeric position when a series: filter pins the result set.
	if err := ensureColumn(db, "images", "series_order", `ALTER TABLE images ADD COLUMN series_order INTEGER`); err != nil {
		return err
	}
	if _, err := db.Write.Exec(`CREATE INDEX IF NOT EXISTS idx_images_series ON images(series) WHERE series != ''`); err != nil {
		return fmt.Errorf("create idx_images_series: %w", err)
	}
	// Saved-search reproduces the URL the operator was looking at; the
	// seed bit lets a `random` save reopen at the same shuffle. `sort_order`
	// is the URL's `order` value - column name is suffixed because `order`
	// is a SQLite reserved word that breaks plain UPDATE/INSERT statements
	// even with quoting in some driver paths.
	if err := ensureColumn(db, "saved_searches", "sort", `ALTER TABLE saved_searches ADD COLUMN sort TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "saved_searches", "sort_order", `ALTER TABLE saved_searches ADD COLUMN sort_order TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "saved_searches", "seed", `ALTER TABLE saved_searches ADD COLUMN seed TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "image_paths", "mtime_unix", `ALTER TABLE image_paths ADD COLUMN mtime_unix INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
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
	var userVersion int
	if err := db.Write.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if userVersion < bootstrapSchemaVersion {
		if _, err := db.Write.Exec(`PRAGMA analysis_limit = 400`); err != nil {
			return fmt.Errorf("set analysis_limit: %w", err)
		}
		if _, err := db.Write.Exec(`ANALYZE images`); err != nil {
			return fmt.Errorf("analyze images: %w", err)
		}
		if _, err := db.Write.Exec(`ANALYZE image_tags`); err != nil {
			return fmt.Errorf("analyze image_tags: %w", err)
		}
		if _, err := db.Write.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, bootstrapSchemaVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	}
	if _, err := db.Write.Exec(`PRAGMA optimize`); err != nil {
		return fmt.Errorf("pragma optimize: %w", err)
	}
	return nil
}

// ensureColumn adds a column on the named table when it is absent. The
// caller supplies the full ALTER TABLE so the default and type stay
// adjacent to the original schema definition.
func ensureColumn(db *DB, table, column, alterSQL string) error {
	var count int
	if err := db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
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

// UntaggedVisibleCount returns the cached count of visible images that
// carry no image_tags row, or queries it on demand. fastCountTagged
// subtracts this from the visible total to derive an exact tagged:true
// partition without re-walking image_tags on every search.
func (db *DB) UntaggedVisibleCount() (int, bool) {
	if p := db.untaggedVisible.Load(); p != nil {
		return *p, true
	}
	var n int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)`,
	).Scan(&n); err != nil {
		return 0, false
	}
	db.untaggedVisible.Store(&n)
	return n, true
}

// AutoUntaggedVisibleCount is UntaggedVisibleCount restricted to
// image_tags rows carrying is_auto = 1 - the subtrahend behind
// autotagged:true. There is no covering (image_id, is_auto) index, so
// the NOT-EXISTS walk is heavier than the bare-untagged one above.
func (db *DB) AutoUntaggedVisibleCount() (int, bool) {
	if p := db.autoUntaggedVisible.Load(); p != nil {
		return *p, true
	}
	var n int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM image_tags it
		         WHERE it.image_id = i.id AND it.is_auto = 1
		       )`,
	).Scan(&n); err != nil {
		return 0, false
	}
	db.autoUntaggedVisible.Store(&n)
	return n, true
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
			c.Close()
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
