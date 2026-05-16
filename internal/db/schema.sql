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
    -- Video duration in seconds (REAL so short clips and sub-second
    -- precision survive). NULL for non-video rows and for video rows
    -- that pre-date the column or whose ffprobe call failed; the
    -- search and detail surfaces treat NULL as "unknown" rather than
    -- "zero". Backfilled on re-extract metadata.
    duration_seconds REAL,
    -- 64-bit canonical perceptual hash (DCT-based pHash, mirror-
    -- canonicalised). NULL until backfilled or when the file has no
    -- visual surface. Added by ensureColumn on existing libraries;
    -- the matching idx_images_phash is created in db.Bootstrap.
    phash          INTEGER,
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

-- Duplicate group: a set of images representing the same source content
-- in different quality / format. One member is the "original" (the best
-- representative). original_image_id is NOT NULL and has no ON DELETE
-- cascade so the parent image_delete path is forced to fix the original
-- (or dissolve the group) before the image row goes away - prevents the
-- group from outliving its anchor as a dangling reference.
CREATE TABLE IF NOT EXISTS dup_groups (
    id                INTEGER PRIMARY KEY,
    original_image_id INTEGER NOT NULL REFERENCES images(id),
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS dup_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES dup_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_groups (
    id         INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES alt_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Directed version edge. child_image_id is PK (each image has at most
-- one parent); parent_image_id is UNIQUE (each parent has at most one
-- child). Together this enforces a strict chain - branching is a
-- derivative relationship.
CREATE TABLE IF NOT EXISTS version_edges (
    child_image_id  INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    parent_image_id INTEGER NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Directed derivative edge. derivative_image_id is PK (a derivative has
-- exactly one source); source_image_id is unconstrained so a source can
-- carry many derivatives (tree).
CREATE TABLE IF NOT EXISTS derivative_edges (
    derivative_image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    source_image_id     INTEGER NOT NULL    REFERENCES images(id) ON DELETE CASCADE,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Canonicalised "not related" pair (a < b). Recorded so a rejected pair
-- never resurfaces in the find-pairs queue at any distance.
CREATE TABLE IF NOT EXISTS not_related_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (a_image_id, b_image_id)
);

-- Singleton holding the active session's order mode plus the
-- started-at timestamp. id is constrained to 1 so there is at most
-- one row regardless of how the upserts go.
CREATE TABLE IF NOT EXISTS relation_session (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    order_mode TEXT NOT NULL DEFAULT 'smallest_distance_first',
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    paused_at  TEXT
);

-- Candidate pairs surfaced by the find-pairs background job; the
-- session UI iterates these and either commits a relation (deletes
-- the row), rejects the pair (deletes the row + writes
-- not_related_pairs), or skips (sets skipped_at so the row sorts to
-- the back of the queue). Canonicalised a < b matches the rest of the
-- symmetric tables.
CREATE TABLE IF NOT EXISTS potential_relation_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    distance   INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    skipped_at TEXT,
    PRIMARY KEY (a_image_id, b_image_id)
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
-- Partial visible indexes over columns the original schema already
-- carries (file_type, source_type) so mime: / type: / ai: filters
-- seek the visibility-bounded set instead of falling back on
-- idx_images_missing. idx_images_source_visible and
-- idx_images_duration_visible reference columns added by ensureColumn
-- migrations (source, duration_seconds) and live in db.Bootstrap
-- below the matching ensureColumn call - adding them here would
-- error on libraries that predate the columns.
CREATE INDEX IF NOT EXISTS idx_images_file_type_visible   ON images(file_type)   WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_source_type_visible ON images(source_type) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_image_paths_image ON image_paths(image_id);
CREATE INDEX IF NOT EXISTS idx_sd_metadata_genhash      ON sd_metadata(generation_hash)      WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_genhash ON comfyui_metadata(generation_hash) WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sd_metadata_seed         ON sd_metadata(seed)                 WHERE seed IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_seed    ON comfyui_metadata(seed)            WHERE seed IS NOT NULL;
-- Relations covering indexes. The PRIMARY KEY on dup_group_members.image_id
-- and alt_group_members.image_id already covers the per-image group lookup;
-- the (group_id, image_id) shape below covers the inverse - listing a
-- group's members ordered. version_edges and derivative_edges get inverse
-- indexes from the parent / source side. not_related_pairs picks up an
-- index on (b, a) so pair-existence checks ride a covering seek regardless
-- of which side the caller passed first.
CREATE INDEX IF NOT EXISTS idx_dup_group_members_group ON dup_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_alt_group_members_group ON alt_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_derivative_edges_source ON derivative_edges(source_image_id);
CREATE INDEX IF NOT EXISTS idx_not_related_b           ON not_related_pairs(b_image_id, a_image_id);
CREATE INDEX IF NOT EXISTS idx_potential_pairs_distance ON potential_relation_pairs(skipped_at, distance, a_image_id);
