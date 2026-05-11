-- Monbooru Schema
-- All statements use IF NOT EXISTS / INSERT OR IGNORE for idempotency.

CREATE TABLE IF NOT EXISTS tag_categories (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    color      TEXT    NOT NULL DEFAULT '#888888',
    is_builtin INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO tag_categories (name, color, is_builtin) VALUES
    ('general',   '#3d90e3', 1),
    ('character', '#00aa00', 1),
    ('artist',    '#cc0000', 1),
    ('copyright', '#aa00aa', 1),
    ('meta',      '#ffaa00', 1),
    ('rating',    '#996666', 1),
    ('medium',    '#7d4fbf', 1),
    ('person',    '#b85c9e', 1),
    ('year',      '#4a8fa8', 1);

-- Promote any pre-existing user-created medium/person/year category to
-- built-in so a library that already had one of these as a custom row
-- stops being deletable once the seed catches up.
UPDATE tag_categories SET is_builtin = 1 WHERE name IN ('medium', 'person', 'year');

CREATE TABLE IF NOT EXISTS tags (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    category_id      INTEGER NOT NULL REFERENCES tag_categories(id),
    usage_count      INTEGER NOT NULL DEFAULT 0,
    is_alias         INTEGER NOT NULL DEFAULT 0,
    canonical_tag_id INTEGER REFERENCES tags(id),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(name, category_id)
);

-- Canonical rating tags. The category accepts only these four names; the
-- tagger routes WD14 rating labels here, search uses the IDs directly via
-- a fixed-name SELECT, and the GetOrCreateTag guard refuses anything else
-- in this category.
INSERT OR IGNORE INTO tags (name, category_id) VALUES
    ('general',      (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('sensitive',    (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('questionable', (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('explicit',     (SELECT id FROM tag_categories WHERE name = 'rating'));

CREATE TABLE IF NOT EXISTS images (
    id             INTEGER PRIMARY KEY,
    sha256         TEXT    NOT NULL UNIQUE,
    canonical_path TEXT    NOT NULL,
    folder_path    TEXT    NOT NULL DEFAULT '',
    file_type      TEXT    NOT NULL,
    width          INTEGER,
    height         INTEGER,
    file_size      INTEGER NOT NULL,
    is_missing     INTEGER NOT NULL DEFAULT 0,
    is_favorited   INTEGER NOT NULL DEFAULT 0,
    -- New ingests land in the inbox (1) for triage; archived rows sit at 0.
    -- Matching idx_images_inbox_visible is created in db.go Bootstrap so the
    -- migration's ALTER TABLE on existing libraries runs before the index
    -- references the new column.
    is_inbox       INTEGER NOT NULL DEFAULT 1,
    auto_tagged_at TEXT,
    source_type    TEXT    NOT NULL DEFAULT 'none',
    origin         TEXT    NOT NULL DEFAULT 'ingest',
    source         TEXT    NOT NULL DEFAULT '',
    url            TEXT    NOT NULL DEFAULT '',
    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS image_paths (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,
    is_canonical INTEGER NOT NULL DEFAULT 0,
    -- File mtime (Unix seconds) at the time the row was last touched.
    -- Sync's unchanged-shortcut requires (size, mtime) parity so a
    -- same-size in-place edit is still re-hashed. 0 marks rows that
    -- predate this column on upgraded libraries; sync re-hashes them
    -- once and writes the real mtime back.
    mtime_unix   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS image_tags (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
    is_auto     INTEGER NOT NULL DEFAULT 0,
    is_implied  INTEGER NOT NULL DEFAULT 0,
    confidence  REAL,
    tagger_name TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (image_id, tag_id)
);

CREATE TABLE IF NOT EXISTS tag_implications (
    parent_tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    implied_tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (parent_tag_id, implied_tag_id)
);

CREATE TABLE IF NOT EXISTS sd_metadata (
    image_id        INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt          TEXT,
    negative_prompt TEXT,
    model           TEXT,
    seed            INTEGER,
    sampler         TEXT,
    steps           INTEGER,
    cfg_scale       REAL,
    raw_params      TEXT,
    generation_hash TEXT
);

CREATE TABLE IF NOT EXISTS comfyui_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt           TEXT,
    model_checkpoint TEXT,
    seed             INTEGER,
    sampler          TEXT,
    steps            INTEGER,
    cfg_scale        REAL,
    raw_workflow     TEXT,
    generation_hash  TEXT
);

CREATE TABLE IF NOT EXISTS manga_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    title            TEXT,
    series           TEXT,
    number           TEXT,
    volume           TEXT,
    count            INTEGER,
    summary          TEXT,
    notes            TEXT,
    year             INTEGER,
    month            INTEGER,
    day              INTEGER,
    writer           TEXT,
    penciller        TEXT,
    inker            TEXT,
    colorist         TEXT,
    letterer         TEXT,
    cover_artist     TEXT,
    editor           TEXT,
    publisher        TEXT,
    imprint          TEXT,
    genre            TEXT,
    web              TEXT,
    language_iso     TEXT,
    format           TEXT,
    manga            TEXT,
    age_rating       TEXT,
    community_rating REAL,
    xml_page_count   INTEGER,
    raw_xml          TEXT
);

CREATE TABLE IF NOT EXISTS saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    query      TEXT NOT NULL,
    sort       TEXT NOT NULL DEFAULT '',
    sort_order TEXT NOT NULL DEFAULT '',
    seed       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_tags_name         ON tags(name);
CREATE INDEX IF NOT EXISTS idx_tags_category     ON tags(category_id);
CREATE INDEX IF NOT EXISTS idx_tags_usage        ON tags(usage_count DESC);
CREATE INDEX IF NOT EXISTS idx_tags_active_usage ON tags(usage_count DESC, name) WHERE is_alias = 0;
CREATE INDEX IF NOT EXISTS idx_tags_alias_canonical ON tags(canonical_tag_id, name) WHERE is_alias = 1;
-- Composite covering index: a `tag_id = ?` lookup gets `image_id`
-- straight from the index entry, so the multi-leg INTERSECT in the
-- AND-driver doesn't pay one row-fetch per matched image. The
-- `image_id` suffix also makes `tag_id = ? AND image_id >= ?` a real
-- range seek for the recent-id-bounded INTERSECT shape. This index
-- supersedes the older single-column `idx_image_tags_tag(tag_id)`;
-- Bootstrap drops that one explicitly when upgrading.
CREATE INDEX IF NOT EXISTS idx_image_tags_tag_image ON image_tags(tag_id, image_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_image  ON image_tags(image_id);
CREATE INDEX IF NOT EXISTS idx_tag_implications_implied ON tag_implications(implied_tag_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_user_tag ON image_tags(tag_id) WHERE is_auto = 0;
CREATE INDEX IF NOT EXISTS idx_image_tags_auto_tagger ON image_tags(tagger_name)
    WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != '';
CREATE INDEX IF NOT EXISTS idx_images_sha256     ON images(sha256);
CREATE INDEX IF NOT EXISTS idx_images_ingested   ON images(ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_favorited  ON images(is_favorited);
CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type);
-- idx_images_source(source) is created in db.Bootstrap so the migration's
-- ALTER TABLE ADD COLUMN source on libraries that predate the column
-- runs before this index references it (schema.sql is executed before
-- ensureColumn).
CREATE INDEX IF NOT EXISTS idx_images_missing    ON images(is_missing);
CREATE INDEX IF NOT EXISTS idx_images_folder     ON images(folder_path);
CREATE INDEX IF NOT EXISTS idx_images_folder_visible ON images(folder_path) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_filesize_visible ON images(file_size DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_ingested_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_image_paths_image ON image_paths(image_id);
CREATE INDEX IF NOT EXISTS idx_sd_metadata_genhash      ON sd_metadata(generation_hash)      WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_genhash ON comfyui_metadata(generation_hash) WHERE generation_hash IS NOT NULL;
