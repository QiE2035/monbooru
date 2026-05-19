# Relations

Monbooru tracks five kinds of operator-declared relationships between
images, plus a "not related" rejection list so a pair never resurfaces:

- **Duplicate** - same image, different files. Members live in an
  unranked group with a chosen *original* (the canonical
  representative).
- **Alternate** - same subject, different rendering. Members live in
  an unranked group; no member is privileged.
- **Version** - directed `parent -> child` edge. Each image has at
  most one parent and at most one child (strict chain, not a tree).
- **Derivative** - directed `source -> derivative` edge. A derivative
  has exactly one source; a source can carry many derivatives (tree).
- **Not related** - a rejected pair recorded so the find-pairs queue
  never surfaces it again.

Collections (`images.series` plus an optional within-collection
order) are a parallel grouping mechanism. They show up alongside
relations on the detail page and search via `collection:<value>` and
`relation:collection`, but they are not part of the duplicate /
alternate / version / derivative graph.

## Backfill perceptual hashes

The Relations features ride a 64-bit per-image perceptual hash
(DCT-based, mirror-canonicalised so a horizontally flipped copy
hashes to the same value). New ingests get the hash automatically.
On an existing library:

1. Open **Settings -> Maintenance**.
2. Click **Compute perceptual hashes**. Runs a background job that
   walks every image still missing a `phash`.

The number of images currently lacking a phash is also shown on the
Relations hub (under "Find relations") as "**N** image(s) without a
phash".

## Find candidate pairs

The find-pairs job populates a queue (`potential_relation_pairs`)
the session UI walks. Trigger it from either:

- **Relations -> Find new pairs** - adds new candidates from images
  hashed since the last scan.
- **Settings -> Maintenance -> Rebuild pair queue** - wipes the
  existing queue (including skipped rows) and rescans from scratch.

Both run the same job. The Hamming-distance cutoff comes from
**Settings -> Relations -> Find-pairs default distance** (default 4,
range 0..12); set it tighter for fewer, more confident pairs.

The job also probes incrementally: when a fresh image is ingested,
monbooru searches the in-memory BK-tree for near-duplicates and
queues any hits without waiting for the next manual scan. Toggling
this off via the TOML config is possible (`relations.incremental_on_ingest`)
but the value resets to `true` on every settings save.

## Triage with the session UI

**Relations -> Start a session** opens the swipe view. Each pair is
rendered side-by-side with the Hamming distance and the larger
filesize on the left (a hint that bigger usually means "more
canonical"). 

Decisions commit in one transaction. A merge that drops the pair's
co-grouped queue rows also sweeps `not_related_pairs` so the session
moves to the next candidate immediately.

Session ordering is set under **Settings -> Relations**:

- **Smallest distance first** (default) - work the most-confident
  candidates first.
- **Largest file first** - useful when the goal is to merge into the
  best-quality original.
- **Random** - shuffle the queue.

## Clean up duplicates

Two parallel views, each with its own walker:

- **Marked duplicates** (`/relations/duplicates/marked`) - declared
  duplicate groups. Each row shows the original and one non-original
  member; **Delete** removes the non-original from the gallery. The
  **Copy tags** button (when relevant) layers the duplicate's tags
  onto the original before the delete. **Delete all duplicate
  images** removes every non-original member of every marked group.
- **SHA-256 duplicates** (`/relations/duplicates/sha256`) - file-level
  alias paths: one byte-identical image stored at multiple paths on
  disk. **Delete** removes the duplicate path and its file; the canonical
  path is kept. **Delete all duplicate files** runs this across every
  alias.

Both walkers are also linked from the Relations hub under
**Duplicates**. They are deliberately separate features: a SHA-256
duplicate is a filesystem-level alias (already one image in the DB),
while a marked duplicate is a relation-level grouping of distinct
DB rows.

## Schedule

**Settings -> Schedule -> Find relation pairs (phash near-duplicates)**
runs the find-pairs job at the configured daily time, scoped to every
configured gallery in turn. Off by default. The phash backfill is not
scheduled; run it once after upgrading, then ingest covers the rest.
