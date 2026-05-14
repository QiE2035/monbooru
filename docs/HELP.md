# Monbooru help

## Getting started

First launch with the Docker compose file:

1. Edit the volume paths in `docker/docker-compose.yml` so `/gallery`, `/data`, `/config`, and `/models` map to host paths you control.
2. `docker compose up -d`.
3. Open `http://localhost:8080`.

The config file is created with defaults at `/config/monbooru.toml` on first start. Most settings are editable from the Settings page; the few that aren't (paths, bind address) need a TOML edit and a restart.

A gallery is a named folder full of images. The default one points at `/gallery`. Drop a file under that path and the watcher picks it up within a few seconds. If the watcher is off, hit **Sync** from the gallery header.

---

## Adding images

Three ways:

- **Drop files into the gallery folder** - the watcher ingests them. 
- **Upload page** (`/upload`) - multi-file browser upload with optional tags and an optional destination subfolder.
- **REST API** - `POST /api/v1/images` with a multipart `file`, or a JSON `{path, tags, folder}`. See the REST API section below.

Duplicates (same SHA-256) are recognised: a second copy under a different path is recorded as an alias of the original, not a new image.

If you copy in a lot of files at once, the watcher can miss some events. Run **Sync** from the gallery header afterwards to catch up.

---

## Tagging

On the image detail page, the tag input takes one or more space-separated tokens:

- `1girl blue_eyes` - adds two tags in the `general` category.
- `artist:john_doe` - adds the tag in a specific category.
- `"red hair" blue_eyes` - quotes group multiple words into one tag (spaces become underscores).
- `artist:"john doe"` - category prefix plus quoted name.

Removing the last image that uses a tag drops `usage_count` to 0 but keeps the tag itself, so user-declared aliases and implications survive an empty library. Use the Tags page Delete action to delete the tag when you actually want it gone; the **Zero-usage** filter is `Show` by default so those rows stay visible, flip it to `Hide` to scope the listing to applied tags, or `Only` for the zero-usage triage view.

Allowed characters: lowercase `a-z`, `0-9`, and `_ ( ) ! @ # $ . ~ + - : ? < > = ^`. Max 200 chars.

Batch tagging:

- **Tag selected** - check thumbnails in the gallery, the batch bar shows up.
- **Tag all** - applies to every image matching the current search.

Both open a dialog with an Add/Remove toggle and the same tag input syntax as above.

---

## Source, URL, Collection, Order

Each image carries four user-edited free-form fields next to the metadata panel on the detail page. Click `[edit]` next to any of them to set them.

- **Source** - provenance label (a site name, scraper, anything you want to remember). Surfaces in the `source:my_label` search filter (exact match). Bare `source:` matches images with nothing set. Max 200 chars.
- **URL** - the canonical web URL the image came from. Must start with `http://` or `https://`. Rendered as a clickable link (new tab) on the detail page. Max 2048 chars.
- **Collection** - grouping label shared by every image you want to keep together (a series name, a comic, a photoshoot). Surfaces in the `collection:"my label"` search filter (exact match) and as a Collections section in the gallery sidebar; the `Order` sort groups by collection first. Cbz / zip uploads pre-fill this from `ComicInfo.xml` `<Series>` when present; re-extract never overwrites a non-empty value. Max 200 chars.
- **Order** - position of this image inside its collection (e.g. page or chapter number). Renders as `#N` next to the collection chip and drives the within-collection ordering of the `Order` sort. NULL by default; the batch dialog can fan a starting integer across a selection so a freshly-labelled batch lands ordered.

All four default to empty; leave them blank if you don't track this.

---

## Tag categories and merging

Nine built-in categories: `general`, `character`, `artist`, `copyright`, `meta`, `rating`, `medium`, `person`, `year`. Manage categories at `/categories`: add new ones with their own color, rename, recolor, or delete. Deleting a custom category prompts to either move its tags to another category or delete them all.

The `rating` category is locked to its four canonical names (see below).

**Reserved category names.** A handful of names are refused at create / rename time because they double as search-filter prefixes and would collide with `category:tag` parsing: `fav`, `inbox`, `ai`, `source`, `cat`, `width`, `height`, `date`, `missing`, `tagged`, `autotagged`, `folder`, `folderonly`, `generated`, `rating`, `type`, `collection`, `pages`, plus `system` (the search-bar cheat-sheet trigger).

**Aliasing a tag** is on the `/tags` page: pick a non-alias row and click `Alias→`. The dialog asks for the canonical to point at. After submit:

- Image-tag rows move to the canonical tag.
- The aliased tag stays in the table with `0` usage and an `alias` badge.
- Anything typed later that matches the alias name resolves to the canonical (in tag input, search, autocomplete, REST API).

**Direct alias creation** uses the **Create alias…** button at the top of the `/tags` page when the alias name isn't on any image yet. Pick a name + category and the existing tag it should resolve to. If the name already names a tag with images attached, the dialog tells you to use the row's `Alias→` button instead (which moves those rows onto the canonical first).

**Repointing** an existing alias uses the same dialog, opened from the alias row's `Repoint→` button - it switches the alias to a different canonical without touching anything else.

Alias rows show as `alias_name → canonical_name` in the Name column. Filter the listing to alias-only via the Origin dropdown.

**Tag implications** are declared per-tag from the `Implications…` button on a non-alias row. Each edge says "adding `parent` to an image also adds `implied` to that image". Implied rows and any aliases pointing at on-image tags share a single dim collapsed `Implied tags / Aliases` section at the bottom of the image's tag list. The per-image sidebar excludes implied rows so it stays focused on tags you applied. Manually re-adding an implied tag converts it to user-owned so removing the parent later won't sweep it. Adding or removing an implication kicks off a background job to retroactively fan it out (or sweep) across every image already carrying the parent. Cycles are refused at create time.

---

## Rating and SFW ceiling

Each image carries at most one rating tag, from the canonical set:

```
general < sensitive < questionable < explicit
```

The auto-tagger keeps the highest rank when an inference emits more than one rating output. A manual edit (detail-page tag input, batch tag, REST API tag-add) overwrites whatever rating was there with the level you typed, even if it ranks below the existing one. `rating:explicit` matches images whose effective rating is exactly `explicit`.

The footer carries the **SFW ceiling**: a row labelled `rating: sfw · sensitive · questionable · explicit`. Click any level to ceiling the gallery to that and below. The active level renders as `[label]`. Default is `[explicit]` (no ceiling). The setting is stored in a HttpOnly cookie scoped to the current browser.

The folder tree ignore the ceiling - it always shows the true gallery shape - so you can tell when something is hidden.

---

## Search

See the in-app help for search syntax.

**Sort:** newest (default), file size, order, random shuffle. Order groups by collection alphabetically then by the per-image order field (NULLs last; images with no collection sit at the start). Random stays stable across page turns; click again to re-shuffle.

**Saved searches:** open the gallery **Actions** chooser (button next to the search bar, or press `a`) and pick **Save search** to store the current query under a name. The entry appears in the sidebar's Saved searches section; click it to re-run, × to delete.

**Favorites:** press `f` on the detail page or click the heart. Search with `fav:true`.

**Inbox/archive:** every newly-ingested image lands in the inbox (untriaged). Press `i` on the detail page to flip it to archived (curated), or use the gallery's batch surface to send a whole search to/from the inbox. The toolbar's `✱` toggle filters the gallery to the inbox. Search with `inbox:true` or `inbox:false`.

---

## Browsing

The gallery sidebar has:

- A tag filter (client-side, restricts the visible tags from the current page).
- Tags from the current page, grouped by category.
- The list of collections in the gallery.
- The folder tree - every folder with a count. Click the name to recurse into the folder; click the small `·` next to it to filter to images directly in that folder, no subfolders.
- AI buttons (a1111 / comfyui / none).
- Saved searches.

The image detail page reuses the folder tree, AI-source buttons, and saved searches in its sidebar; the current image's tag list sits above them.

**Related images:** the bottom of the detail page shows up to 9 images sharing tags with the current one, ranked by overlap.

---

## Comics & manga (Archives)

Drop a `.cbz` or `.zip` archive of page images into your gallery (or upload one via the web upload form) and it ingests as a single library row, just like an image. The cover thumbnail is page 1 (natural-sorted across the archive).

The detail page for a manga gains two extras:
- **Read** - opens a reader at page 1.
- **Pages** - opens a thumbnail grid of every page, click a cell to jump straight to that page in the reader.

`ComicInfo.xml` at the archive root, when present, is parsed into a read-only metadata panel on the detail page (title, series, volume, writer, summary, ...).

Auto-tagging a manga reads every page in the archive, runs each through the configured taggers, and merges the results as one tag set on the comic row.

---

## Keyboard shortcuts

Press `?` on any page to open the cheat-sheet overlay.
---

## Galleries

A gallery is a named folder plus its own SQLite DB and thumbnails. Each gallery is independent - its own images, tags, saved searches, and so on.

Manage them at **Settings → Galleries**:

- **Add** - give it a name and a path on disk. The Add form also accepts an import file (.db / .json / .zip) so you can create a populated gallery in one shot.
- **Switch** - runtime only; doesn't persist.
- **Set default** - the gallery that loads on startup.
- **Rename** / **Delete** - type the gallery name to confirm.

The footer shows the active gallery; click it to switch when more than one is configured.

REST API requests can target any gallery via `?gallery=<name>` or the `X-Monbooru-Gallery` header.

**Export** offers three formats:

- `format=db` - SQLite snapshot (`VACUUM INTO`). Safe against concurrent reads/writes.
- `format=json` - single JSON document with every table.
- `format=light` - portable bundle: a bare `tags.json` listing `{sha256, path, tags}` per image, with monbooru-specific data (SD/ComfyUI metadata, saved searches, tag attribution) stripped. Useful as a tags-only backup or as input for other software.

`with_images=true` bundles either format plus every file under the gallery into a `.zip`. Thumbnails are deliberately excluded; rebuild via Maintenance afterwards. For `format=light` without `with_images`, the response is the bare `tags.json`.

**Import** accepts a `.db`, `.sqlite`, `.json`, or `.zip`, plus two foreign-format zips:

- A Blombooru `full_backup.zip`.
- A zipped Hydrus export folder (media files plus `.txt` sidecars).

A `.json` upload may be either a full monbooru export or a bare `tags.json` light manifest; the importer sniffs the document and routes accordingly. For the foreign formats and the light manifest, only image bytes and tags are preserved. See `docs/MIGRATING.md` for details.

The dialog has two modes:

- **Merge** (default) - add new images and tags from the upload to the target gallery. Existing rows are kept; tags from the upload are layered onto matching SHA-256 rows.
- **Replace** - wipe the target gallery's DB and thumbnails, then load the upload into the empty gallery. Type-to-confirm the gallery's name. Refused on the active gallery and the default gallery (switch / demote first).

---

## Auto-tagger

The auto-tagger uses ONNX models to suggest tags for your images. Off by default.

**Install a supported tagger from the catalog:**

1. Open Settings → Auto-Tagger. The table lists available taggers (WD14 SwinV2, JoyTag, Camie v2).
2. Click "Show instructions" on the row you want. The dialog has shell snippets for both host install and `docker exec` install.
3. Run the snippet on a machine with internet access. Monbooru itself never reaches out.
4. Tick `Enabled` on the row.

**Install a custom ONNX model:**

Other custom ONNX model may or may not work.
Drop the model into its own subfolder under the `models/` volume. Each subfolder needs:

- `model.onnx` - the weights.
- One label file: `tags.csv` (WD14 schema: `tag_id,name,category_id`), `tags.txt` (one label per line, all `general`), or a Camie-style metadata `.json` (`dataset_info.tag_mapping.idx_to_tag` + `tag_to_category`).

Reload the Settings page; the new tagger appears in the table.

To run it, use the auto-tag button in the image detail or in batch actions.

Multiple taggers can run together; per-image results are merged so a tag detected by two taggers is inserted once with the higher confidence.

**Thresholds and per-category caps:** each tagger has a global confidence threshold plus an optional per-category override map. Open Settings → Auto-Tagger → Configure on a row to edit the global threshold, add overrides for individual categories (e.g. raise `character` to 0.85 to suppress false-positive character tags while keeping `general` permissive), and set a **Max tags** per category - the maximum number of tags this tagger may emit for that category on one image after thresholding. Empty Max tags cells fall back to the built-in defaults (`character` 8, `copyright` 4, `artist` 4, `general` 25, `rating` 1, anything else 10); `0` keeps every tag that survives the threshold. Empty per-category threshold cells fall back to the global threshold; click Reset to drop an override.

**Frame-merge gate (videos and manga only):** when a tagger runs against a video (5 sampled frames) or a manga (every page), monbooru merges per-frame scores into a single set of tags per image. The `tagger.aggregation.min_hit_fraction` TOML knob (default `0.05`) controls how many frames a label must score above the threshold on to survive the merge: the cutoff is `clamp(ceil(min_hit_fraction × frame_count), 2, 10)`. A single noisy hit on a 200-page manga is not enough; the same label appearing on 10+ pages does survive. Set the knob to `0` to revert to "any single hit wins". Static images are unaffected (always single-frame).

**Per-gallery enabling:** each tagger row has a Galleries column with a Configure button. Tick "All galleries" so the tagger fires on every gallery (default). Tick individual galleries to restrict it to just those - useful when one gallery holds anime work and another holds photos and you don't want WD14 firing on the photos.

**Override label routing (advanced):** drop a `dispatch.json` next to the tagger's `model.onnx` to remap a label to another category, rename it, or drop it entirely. Format and the shipped defaults are at `internal/tagger/dispatch_default/<tagger>.json`.

**GPU (CUDA):** the default image is CPU-only (~210 MB). For GPU inference, switch to the `-cuda` image (~2.3 GB), pass the GPU into the container the usual way, then enable Settings → Auto-Tagger → Use GPU (CUDA) (or set `MONBOORU_TAGGER_USE_CUDA=true`). The current mode is shown as a badge. Worker count is configurable from Settings → Auto-Tagger or `tagger.parallel` in TOML (default 4); raise it on GPU if preprocessing becomes the bottleneck.

The model stays loaded for 30 minutes after the last run, then unloads to free memory. Tune via `tagger.idle_release_after_minutes`; `0` releases immediately after every run.

---

## Maintenance

Settings → Maintenance has the manual tools:

- **Prune missing images** - delete DB rows for files no longer on disk.
- **Prune orphaned thumbnails** - delete thumbnail files whose image row is gone.
- **Rebuild thumbnails** - regenerate every thumbnail. Useful after import or after a backup restore.
- **Recalculate tag counts** - recompute `usage_count` on every tag (zero-usage rows persist; delete them individually on the Tags page when you want them gone). The watcher and bulk paths should keep counts correct in real time, so you only need this if you see discrepancies in usage count.
- **Merge general tags** - folds an auto-tagger-only general tag into its unique categorized counterpart of the same name (e.g. `general:hatsune_miku` → `character:hatsune_miku` when only one such counterpart exists). General tags carrying any manual `image_tags` row are skipped: an explicit user choice always wins over the merge. Useful after a `.txt` tagger run that emits everything as `general:`.
- **Re-extract metadata** - re-runs SD/ComfyUI metadata extraction on every image.
- **Vacuum database** - `VACUUM` plus WAL checkpoint to release space.
- **Duplicates** - list and remove duplicate file paths.

Each action reports how many rows it affected.

---

## Stats

Settings → Stats is a read-only diagnostic block:

- **Process memory** - PSS broken into Go heap, native (SQLite caches, taggers, CGO), and file-backed (DB pages, binary, shared libs) on Linux. Falls back to `runtime.MemStats` on platforms without `/proc/self/smaps_rollup`.
- **Database size per gallery** - one row per gallery, sum of `monbooru.db` plus its `-wal` / `-shm` sidecars.
- **Filesystem free space** - one row per distinct mount that holds the gallery / data / model directories. Mounts reporting the same totals collapse into a single row.

---

## Schedule

Settings → Schedule turns on a daily run at a chosen time (default 01:00). Tick any of:

- Sync gallery
- Remove orphaned thumbnails
- Run enabled auto-taggers (off by default)
- Merge general tags

The scheduler runs through every gallery in turn. If a job is already running at fire time, the run is skipped silently.

---

## Authentication and REST API

**Password login** is off by default. To enable:

```bash
# On the host:
./monbooru -hash-password 'your-password'
# Or via Docker:
docker exec -it monbooru monbooru -hash-password 'your-password'
```

The flag prints the bcrypt hash of the supplied password and exits. Paste the generated hash into Settings → Authentication, or set `auth.password_hash` in TOML and `auth.enable_password = true`. The login rate-limits per IP with exponential backoff.

**REST API** is disabled by default. Enable it by generating a token in Settings → Authentication. Then:

```bash
curl -H "Authorization: Bearer <token>" \
     "http://localhost:8080/api/v1/images/search?q=1girl"
```

- HTML reference: `/api/v1/docs` (also linked in the footer).
- OpenAPI spec: `/api/v1/openapi.json`.

Endpoints cover image search, single-image metadata, per-image tag listing, image add/delete, image tag add/remove, and global tag listing. All endpoints accept `?gallery=<name>` to target a specific gallery.

---

## Configuration

**Volume layout:**

| Mount | Purpose |
|---|---|
| `/gallery` | Source images |
| `/data` | SQLite DB and thumbnails |
| `/config` | `monbooru.toml` |
| `/models` | ONNX taggers (one subfolder per tagger) |

**Custom CSS.** Drop a `custom.css` next to `monbooru.toml` and set `custom_css = "/config/custom.css"` in [server] to load an extra stylesheet. The path must sit under the config directory, `/config`, or `/data`; anything outside that allowlist is logged at startup and the link is suppressed (a guard against a typo like `/etc/passwd` leaking through `/custom.css`). Symlinks are resolved before the check. The file is served at `/custom.css` and linked from the layout after the bundled `main.css`, so a `:root` block in there wins the cascade and you can retheme without rebuilding.

**Environment variables.** All override the TOML config. Pattern: `MONBOORU_{SECTION}_{KEY}`.

| Variable | Overrides | Type |
|---|---|---|
| `MONBOORU_SERVER_BIND_ADDRESS` | `server.bind_address` | string |
| `MONBOORU_SERVER_BASE_URL` | `server.base_url` | string |
| `MONBOORU_PATHS_DATA_PATH` | `paths.data_path` | string |
| `MONBOORU_PATHS_MODEL_PATH` | `paths.model_path` | string |
| `MONBOORU_GALLERY_WATCH_ENABLED` | `gallery.watch_enabled` | bool |
| `MONBOORU_GALLERY_MAX_FILE_SIZE_MB` | `gallery.max_file_size_mb` | int |
| `MONBOORU_TAGGER_USE_CUDA` | `tagger.use_cuda` | bool |
| `MONBOORU_AUTH_ENABLE_PASSWORD` | `auth.enable_password` | bool |
| `MONBOORU_AUTH_PASSWORD_HASH` | `auth.password_hash` | string |
| `MONBOORU_AUTH_SESSION_LIFETIME_DAYS` | `auth.session_lifetime_days` | int |
| `MONBOORU_AUTH_API_TOKEN` | `auth.api_token` | string |
| `MONBOORU_LOG_LEVEL` | `log.level` | `warn` / `info` / `debug` |

Per-tagger settings (enable, confidence, worker count) live in the Settings UI, not env vars.

**Log levels** (`log.level`):

- `warn` (default) - warnings, errors, and explicit mutations (logins, settings changes).
- `info` - adds one line per non-noisy HTTP request and startup banners.
- `debug` - adds static asset, thumbnail, `/health`, and `/internal/job/status` hits.

---

## Building without Docker

```bash
# CPU only, no auto-tagger
go build -o monbooru ./cmd/monbooru

# With auto-tagger (requires the ONNX Runtime shared library on the system)
CGO_ENABLED=1 go build -tags tagger -o monbooru ./cmd/monbooru

./monbooru -config /path/to/monbooru.toml
```

CLI flags:

- `-config` - path to the TOML config file.
- `-hash-password` - print a bcrypt hash and exit.

For the `-tags tagger` build, `libonnxruntime.so` must be reachable - either on `LD_LIBRARY_PATH` / `/usr/lib`, or via the `ORT_LIB_PATH` env var (absolute path to the `.so`). The Docker image bundles ORT v1.21.0 and does not need this.

---

## Troubleshooting

**Degraded mode banner.** The gallery folder is unreadable. Existing DB rows stay browsable but sync and watcher are off. Fix the path or permissions and restart.

**Missing thumbnails.** Run Maintenance → Rebuild thumbnails. For videos and animated GIF hover previews, ffmpeg must be installed (it ships with the Docker image).

**Watcher hits inotify limit.** Logged as `no space left on device` from `inotify_add_watch`. Raise `fs.inotify.max_user_watches` on the host (not inside the container) and restart. If you see `too many open files` instead, raise `fs.inotify.max_user_instances`. Or disable the watcher in Settings and use Sync manually.

**Permission errors on delete or move.** Monbooru takes ownership (`chown`) of files at ingest time so it can delete or move them later. If files were ingested before this was added, run a Sync - it re-chowns supported files it has just hashed.

**Files added but not visible.** Check that they're under `gallery_path`, that the file size is under `gallery.max_file_size_mb`, and that the extension is one of jpg/jpeg/png/webp/gif/mp4/webm. Then run Sync.

**Login locked out.** The login backoff caps at 30s and entries are swept every 10 minutes. Wait it out, or restart the process to clear all sessions.

**API returns 503 `api_disabled`.** Generate a token in Settings → Authentication.
