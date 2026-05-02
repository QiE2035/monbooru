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

- **Drop files into the gallery folder** - the watcher ingests them. Works for files added by `cp`, rsync, an SD generator that writes directly there, etc.
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

Removing the last image that uses a tag drops `usage_count` to 0 but keeps the tag itself, so user-declared aliases and implications survive an empty library. Use the Tags page Delete action to drop the row when you actually want it gone; flip the **Zero-usage** filter to `Show` to surface every tag at zero usage at once.

Allowed characters: lowercase `a-z`, `0-9`, and `_ ( ) ! @ # $ . ~ + - : ? < > = ^`. Max 200 chars.

Batch tagging:

- **Tag selected** - check thumbnails in the gallery, the batch bar shows up.
- **Tag all** - applies to every image matching the current search.

Both open a dialog with an Add/Remove toggle and the same tag input syntax as above. Settings → Tag actions has the same controls plus library-wide bulk removal (auto-tags only, user tags only, or every tag).

---

## Tag categories and merging

Nine built-in categories: `general`, `character`, `artist`, `copyright`, `meta`, `rating`, `medium`, `person`, `year`. Manage categories at `/categories`: add new ones with their own color, rename, recolor, or delete. Deleting a custom category prompts to either move its tags to another category or delete them all.

The `rating` category is locked to its four canonical names - see below.

**Tag merging** is on the `/tags` page: pick a tag and merge it into another. After the merge:

- Image-tag rows move to the canonical tag.
- The merged tag becomes an alias and stays in the table with `0` usage and an `alias` badge.
- Anything typed later that matches the alias name resolves to the canonical (in tag input, search, autocomplete, REST API).

**Direct alias creation** uses the **Create alias…** button at the top of the `/tags` page when the alias name isn't on any image yet. Pick a name + category and the existing tag it should resolve to. If the name already names a tag with images attached, the dialog tells you to use Merge instead (which moves those rows onto the canonical first).

**Repointing** an existing alias is the same Merge dialog, opened from the alias row's `Repoint→` button - it switches the alias to a different canonical without touching anything else.

Alias rows show as `alias_name → canonical_name` in the Name column. Filter the listing to alias-only via the Origin dropdown.

**Tag implications** are declared per-tag from the `Implications…` button on a non-alias row. Each edge says "adding `parent` to an image also adds `implied` to that image". Implied rows render as a separate dim "Implied tags" subsection on the image's tag list and are excluded from the per-image sidebar so the sidebar stays focused on tags you applied. Manually re-adding an implied tag converts it to user-owned so removing the parent later won't sweep it. Adding or removing an implication kicks off a background job to retroactively fan it out (or sweep) across every image already carrying the parent. Cycles are refused at create time.

---

## Rating and SFW ceiling

Each image can carry one or more rating tags from the canonical set:

```
general < sensitive < questionable < explicit
```

When more than one is attached, the highest wins. `rating:explicit` matches images whose effective rating is exactly `explicit`.

The footer carries the **SFW ceiling**: a row labelled `rating: sfw · sensitive · questionable · explicit`. Click any level to ceiling the gallery to that and below. The active level renders as `[label]`. Default is `[explicit]` (no ceiling). The setting is stored in a HttpOnly cookie scoped to the current browser.

The folder tree and source counts ignore the ceiling - they always show the true gallery shape - so you can tell when something is hidden.

---

## Search

Tags separated by spaces means AND. Everything else stacks on top:

| Syntax | Effect |
|---|---|
| `cat dog` | has both tags |
| `cat OR dog` | either one |
| `-blonde_hair` | exclude |
| `blue*` / `*hair*` | wildcards |
| `fav:true` | favorites only |
| `source:a1111` / `source:comfyui` / `source:none` | by metadata source |
| `source:ai` | any image with a1111 and/or comfyui metadata |
| `folder:2024/january` | images in this folder or any subfolder |
| `folder:"my set 1"` | quote paths that contain spaces |
| `folderonly:2024/january` | only images directly in this folder, no subfolders |
| `width:>=1920` `height:<768` | dimensions |
| `date:2024-03-15` `date:>2024-01-01` `date:2024-01..2024-06` | dates |
| `cat:character` | any tag in that category |
| `character:cat` | tag "cat" in the character category |
| `missing:true` | files gone from disk |
| `animated:true` / `animated:false` | gif/mp4/webm |
| `tagged:true` / `tagged:false` | images with or without tags |
| `autotagged:true` / `autotagged:false` | images with or without auto-tags |
| `generated:abcd1234abcd` | same generation recipe (hash shown on the image page) |
| `rating:explicit` | effective rating one of `general` / `sensitive` / `questionable` / `explicit` |
| `system:` (autocomplete only) | cheat-sheet trigger. Lists every filter prefix and tag category (`fav:`, `date:`, ..., `character:`, `artist:`, ...). |

Autocomplete is combination-aware: the count next to each suggestion is for the full query, and suggestions that would return zero results are hidden.

**Sort:** newest (default), file size, random shuffle. Random stays stable across page turns; click again to re-shuffle.

**Saved searches:** click "Save search" in the gallery sidebar to store the current query under a name. Click the entry to run it again. Delete with the × button.

**Favorites:** press `f` on the detail page or click the heart. Search with `fav:true`.

---

## Browsing

The gallery sidebar has:

- A tag filter (client-side, restricts the visible tags from the current page).
- Tags from the current page, grouped by category.
- Source buttons (a1111 / comfyui / none).
- The folder tree - every folder with a count. Click the name to recurse into the folder; click the small `·` next to it to filter to images directly in that folder, no subfolders.
- Saved searches.

The image detail page reuses the folder tree, source buttons, and saved searches in its sidebar; the current image's tag list sits above them.

**Related images:** the bottom of the detail page shows up to 9 images sharing tags with the current one, ranked by overlap.

---

## Keyboard shortcuts

**Anywhere**

| Key | Action |
|---|---|
| `s` | Focus search input |
| `Escape` | Close dialog, blur input, clear selection, exit tag-focus mode, or go back |

**Gallery — navigation**

| Key | Action |
|---|---|
| `h` / `l` | Previous / next page |
| Arrows | Navigate the grid |
| `Enter` | Open focused image |
| `Space` | Toggle selection of the focused thumbnail |
| `Ctrl+A` | Select every visible thumbnail |

**Gallery — actions**

| Key | Action |
|---|---|
| `a` | Open the Actions chooser (Save / Tag / Auto-tag / Move / Delete the current search) |
| `1`-`9` | Pick the matching entry in the open Actions chooser |

**Gallery — selection active**

| Key | Action |
|---|---|
| `a` | Add tags to the selection |
| `r` | Remove tags from the selection |
| `t` | Auto-tag the selection |

**Image detail — navigation**

| Key | Action |
|---|---|
| `←` / `→` | Previous / next image |

**Image detail — actions**

| Key | Action |
|---|---|
| `a` | Focus the tag input |
| `r` | Enter tag-focus mode (arrows cycle, `Enter` removes the focused tag, `Escape` exits) |
| `t` | Open the Auto-tag dialog for this image |
| `f` | Toggle favorite |
| `Space` | Play / pause the video |
| `Delete` | Delete image (advances to the next in the current search) |

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

---

## Auto-tagger

The auto-tagger uses ONNX models to suggest tags for your images. Off by default.

**Install a supported tagger from the catalog:**

1. Open Settings → Auto-Tagger. The table lists available taggers (WD14 SwinV2, JoyTag).
2. Click "Show download instructions" on the row you want. The dialog has shell snippets for both host install and `docker exec` install.
3. Run the snippet on a machine with internet access. Monbooru itself never reaches out.
4. Tick `Enabled` on the row.

**Install a custom ONNX model:**

Drop the model into its own subfolder under the `models/` volume. Each subfolder needs:

- `model.onnx` - the weights.
- `tags.csv` (WD14 schema: `tag_id,name,category_id`) or `tags.txt` (one label per line, all assigned to `general`).

Reload the Settings page; the new tagger appears in the table.

**Run it:**

| Action | Effect |
|---|---|
| Run untagged | Tag every image with no auto-tags yet. Per-tagger or global. |
| Run all | Re-tag every image. |
| Per-image | The Auto-tag button on the detail page. |
| Remove auto-tagged | Delete auto-tags for one tagger or all. Manual tags untouched. |

Multiple taggers can run together; per-image results are merged so a tag detected by two taggers is inserted once with the higher confidence.

**Override label routing (advanced):** drop a `dispatch.json` next to the tagger's `model.onnx` to remap a label to another category, rename it, or drop it entirely. Format and the shipped defaults are at `internal/tagger/dispatch_default/<tagger>.json`.

**GPU (CUDA):** the default image is CPU-only (~210 MB). For GPU inference, switch to the `-cuda` image (~2.3 GB), pass the GPU into the container the usual way, then enable Settings → Auto-Tagger → Use GPU (CUDA) (or set `MONBOORU_TAGGER_USE_CUDA=true`). The current mode is shown as a badge. Worker count is configurable; raise it on GPU if preprocessing becomes the bottleneck.

The model stays loaded for 30 minutes after the last run, then unloads to free memory. Tune via `tagger.idle_release_after_minutes`; `0` releases immediately after every run.

---

## Maintenance

Settings → Maintenance has the manual tools:

- **Prune missing images** - delete DB rows for files no longer on disk.
- **Prune orphaned thumbnails** - delete thumbnail files whose image row is gone.
- **Rebuild thumbnails** - regenerate every thumbnail. Useful after import or after a backup restore.
- **Recalculate tag counts** - recompute `usage_count` on every tag (zero-usage rows persist; delete them individually on the Tags page when you want them gone). The watcher and bulk paths should keep counts correct in real time, so you only need this if you see discrepancies in usage count.
- **Merge general tags** - for `.txt` taggers: merges general-category tags into a unique categorized counterpart of the same name (e.g. `general:hatsune_miku` into `character:hatsune_miku` when only one such counterpart exists).
- **Re-extract metadata** - re-runs SD/ComfyUI metadata extraction on every image.
- **Vacuum database** - `VACUUM` plus WAL checkpoint to release space.
- **Duplicates** - list and remove duplicate file paths.

Each action reports how many rows it affected.

**Sync edge case.** Sync skips re-hashing a file when its `(path, size)` already exists in the DB. A re-encoded JPEG that happens to keep the exact byte length will silently keep the previous SHA, so its tags and metadata stay attached even though the bytes changed. Recovery: delete the row from the Tags / image detail page and re-sync, or replace the file with one of a different size.

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
./monbooru -hash-password
# Or via Docker:
docker exec -it monbooru monbooru -hash-password
```

Paste the generated hash into Settings → Authentication, or set `auth.password_hash` in TOML and `auth.enable_password = true`. The login rate-limits per IP with exponential backoff.

**REST API** is disabled by default. Enable it by generating a token in Settings → Authentication. Then:

```bash
curl -H "Authorization: Bearer <token>" \
     "http://localhost:8080/api/v1/images/search?q=1girl"
```

- HTML reference: `/api/v1/docs` (also linked in the footer).
- OpenAPI spec: `/api/v1/openapi.json`.

Endpoints cover search, image add/delete, tag add/remove, and tag listing. All endpoints accept `?gallery=<name>` to target a specific gallery.

`GET /api/v1/tags` hides non-alias tags whose `usage_count` is `0` by default so the listing reflects what is actually applied to images. Pass `show_zero=1` to surface them; the API total then matches the count rendered on the `/tags` UI page.

---

## Configuration

**Volume layout:**

| Mount | Purpose |
|---|---|
| `/gallery` | Source images |
| `/data` | SQLite DB and thumbnails |
| `/config` | `monbooru.toml` |
| `/models` | ONNX taggers (one subfolder per tagger) |

**Custom CSS.** Drop a `custom.css` next to `monbooru.toml` and set `custom_css = "/config/custom.css"` in [server] (any absolute path) to load an extra stylesheet. The file is served at `/custom.css` and linked from the layout after the bundled `main.css`, so a `:root` block in there wins the cascade and you can retheme without rebuilding.

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

---

## Troubleshooting

**Degraded mode banner.** The gallery folder is unreadable. Existing DB rows stay browsable but sync and watcher are off. Fix the path or permissions and restart.

**Missing thumbnails.** Run Maintenance → Rebuild thumbnails. For videos and animated GIF hover previews, ffmpeg must be installed (it ships with the Docker image).

**Watcher hits inotify limit.** Logged as `no space left on device` from `inotify_add_watch`. Raise `fs.inotify.max_user_watches` on the host (not inside the container) and restart. If you see `too many open files` instead, raise `fs.inotify.max_user_instances`. Or disable the watcher in Settings and use Sync manually.

**Permission errors on delete or move.** Monbooru takes ownership (`chown`) of files at ingest time so it can delete or move them later. If files were ingested before this was added, run a Sync - it re-chowns supported files it has just hashed.

**Files added but not visible.** Check that they're under `gallery_path`, that the file size is under `gallery.max_file_size_mb`, and that the extension is one of jpg/jpeg/png/webp/gif/mp4/webm. Then run Sync.

**Login locked out.** The login backoff caps at 30s and entries are swept every 10 minutes. Wait it out, or restart the process to clear all sessions.

**API returns 503 `api_disabled`.** Generate a token in Settings → Authentication.
