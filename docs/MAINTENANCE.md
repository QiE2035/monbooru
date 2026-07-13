# Maintenance, Schedule, Stats, Troubleshooting

## Maintenance

Settings → Maintenance has the manual tools:

- **Prune missing images** - delete DB rows for files no longer on
  disk, and remove their thumbnails and cached pages.
- **Prune orphaned thumbnails** - delete thumbnail files whose image
  row is gone.
- **Rebuild thumbnails** - regenerate every thumbnail. Useful after
  import or after a backup restore. Also re-probes video dimensions
  via ffprobe.
- **Compute perceptual hashes** - backfill `phash` for every image
  that doesn't have one yet. Required reading for the Relations
  find-pairs job; see [RELATIONS.md](RELATIONS.md).
- **Rebuild pair queue** - wipe `potential_relation_pairs` and rescan
  from scratch. Skipped pairs and any queue work in progress are
  dropped.
- **Recalculate tag counts** - recompute `usage_count` on every tag
  (zero-usage rows persist; delete them individually on the Tags page
  when you want them gone). The watcher and bulk paths should keep
  counts correct in real time, so you only need this if you see
  discrepancies in usage count.
- **Category conflicts** - count tags whose name occupies more than one
  category (e.g. `general:nintendo` and `copyright:nintendo`). The split
  is legal but usually means two sources disagreed on a tag's category.
  The count links to the Tags page's Conflicts filter, where both sides can be merged.
- **Re-extract metadata** - re-runs SD/ComfyUI parameter extraction on
  every image, re-probes video duration via ffprobe, and recomputes each
  image's perceptual hash.
- **Vacuum database** - `VACUUM` plus WAL checkpoint to release space.
- **Free memory** - shrink SQLite caches, return the Go heap, release
  the auto-tagger from RAM/VRAM.

Most actions report how many rows they affected; Vacuum and Free memory
report the space reclaimed instead.

To find and remove byte-identical duplicate file paths, use the SHA-256
duplicates walker under **Relations -> Duplicates**; see
[RELATIONS.md](RELATIONS.md).

## Schedule

Settings → Schedule turns on a daily run at a chosen time (default
01:00). Tick any of:

- Sync gallery
- Remove orphaned thumbnails
- Run enabled auto-taggers (off by default)
- Find relation pairs (phash near-duplicates) (off by default)

The scheduler runs through every gallery in turn. If a job is already
running at fire time, the run is skipped silently.

## Stats

Settings → Stats is a read-only diagnostic block:

- **Process memory** - PSS broken into the Go heap, native heap
  (SQLite caches, CGO), and shared file-backed (DB pages, binary,
  shared libs) on Linux, read from `/proc/self/smaps`. Falls back to
  `runtime.MemStats` on non-Linux platforms.
- **Auto-tagger** - current mode (CPU / CUDA), load status, idle
  timer, and (when the subprocess worker is alive) its PSS and
  ONNX/CGO breakdown.
- **Database size per gallery** - one row per gallery, sum of
  `monbooru.db` plus its `-wal` / `-shm` sidecars.
- **Filesystem free space** - one row per distinct mount that holds
  the gallery / database / thumbnails directories. Mounts reporting the
  same totals collapse into a single row.

## Troubleshooting

**Degraded mode banner.** The gallery folder is unreadable. Existing
DB rows stay browsable but sync and watcher are off. Fix the path or
permissions and restart.

**Missing thumbnails.** Run Maintenance → Rebuild thumbnails. For
videos and animated GIF hover previews, ffmpeg must be installed (it
ships with the Docker image).

**Watcher hits inotify limit.** Logged as `no space left on device`
from `inotify_add_watch`. Raise `fs.inotify.max_user_watches` on the
host (not inside the container) and restart. If you see `too many open
files` instead, raise `fs.inotify.max_user_instances`. Or disable the
watcher in Settings and use Sync manually.

**Permission errors on delete or move.** Monbooru takes ownership
(`chown`) of files at ingest time so it can delete or move them later.
If files were ingested before this was added, run a Sync - it
re-chowns supported files it has just hashed.

**Files added but not visible.** Check that they're under
`gallery_path`, that the file size is under
`gallery.max_file_size_mb`, and that the extension is one of
jpg/jpeg/png/webp/gif/mp4/webm/cbz/zip. Then run Sync.

**Login locked out.** The login backoff caps at 30s; a sweeper runs
every 10 minutes and clears entries idle for 5 minutes. Wait it out, or
restart the process to clear all sessions.

**API returns 503 `api_disabled`.** No API token exists yet; generate
one in Settings → Authentication. (The same status also surfaces when no
gallery is active.)
