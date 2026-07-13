# Installing

## Quick start (Docker)

1. Edit the volume paths in `docker/docker-compose.yml` so `/gallery`,
   `/data`, `/config`, and `/models` map to host paths you control.
2. `docker compose up -d`.
3. Open `http://localhost:8080`.

The config file is created with defaults at `/config/monbooru.toml` on
first start. Most settings are editable from the Settings page; the few
that aren't (paths, bind address) need a TOML edit and a restart.

A gallery is a named folder full of images. The default one points at
`/gallery`. Drop a file under that path and the watcher picks it up
within a few seconds. If the watcher is off, hit **Sync** from the
gallery header.

## Volume layout

| Mount | Purpose |
|---|---|
| `/gallery` | Source images |
| `/data` | SQLite DB and thumbnails |
| `/config` | `monbooru.toml` |
| `/models` | ONNX taggers (one subfolder per tagger) |

## Permissions (non-root)

The image runs as a non-root user (uid/gid `1000`). The bundled
`docker-compose.yml` and Quadlet unit set `1000:1000` to match the common host
user (edit it to your own uid/gid) so files written to `/gallery`, `/data`, and
`/config` land owned by you and the bind mounts stay read/writable. Make sure
those host directories are owned by, or accessible to, that uid.

## Custom CSS

Drop a `custom.css` next to `monbooru.toml` and set
`custom_css = "/config/custom.css"` in `[server]` to load an extra
stylesheet. The path must sit under the config directory, `/config`, or
`/data`; anything outside that allowlist is logged at startup and the
link is suppressed. Symlinks are resolved before the check. The file
is served at `/custom.css` and linked from the layout after the bundled
`main.css`, so a `:root` block in there wins the cascade and you can
retheme without rebuilding.

## Change booru name and logo

You can use these optional keys under `[server]` :

```toml
[server]
name = "monbooru"
logo = "/config/logo.png"
```

`name` replaces the wordmark, every page `<title>`, and the login
heading. The CSS uppercases the wordmark and login heading regardless of
case, so `monbooru` renders as `MONBOORU` in the topbar. Missing or
empty falls back to `Monbooru`.

`logo` is an absolute path to an image file (PNG works; any format the
browser handles is fine). When set, the file is served at `/custom.logo`
and used for both the favicon and the topbar logo. Missing or empty
falls back to the bundled defaults.

## Link to monloader

If you run a [monloader](https://github.com/leqwin/monloader) instance, set its browser URL in the settings. Monbooru learns monloader's API address from the pairing, so the api url field can stay empty.

## Environment variables

All override the TOML config. Pattern: `MONBOORU_{SECTION}_{KEY}`.

| Variable | Overrides | Type |
|---|---|---|
| `MONBOORU_SERVER_BIND_ADDRESS` | `server.bind_address` | string |
| `MONBOORU_SERVER_BASE_URL` | `server.base_url` | string |
| `MONBOORU_SERVER_MONLOADER_URL` | `server.monloader_url` | string |
| `MONBOORU_MONLOADER_API_URL` | `monloader.api_url` | string |
| `MONBOORU_MONLOADER_API_TOKEN` | `monloader.api_token` | string |
| `MONBOORU_PATHS_DATA_PATH` | `paths.data_path` | string |
| `MONBOORU_PATHS_MODEL_PATH` | `paths.model_path` | string |
| `MONBOORU_GALLERY_WATCH_ENABLED` | `gallery.watch_enabled` | bool |
| `MONBOORU_GALLERY_MAX_FILE_SIZE_MB` | `gallery.max_file_size_mb` | int |
| `MONBOORU_TAGGER_USE_CUDA` | `tagger.use_cuda` | bool |
| `MONBOORU_AUTH_ENABLE_PASSWORD` | `auth.enable_password` | bool |
| `MONBOORU_AUTH_PASSWORD_HASH` | `auth.password_hash` | string |
| `MONBOORU_AUTH_SESSION_LIFETIME_DAYS` | `auth.session_lifetime_days` | int |
| `MONBOORU_LOG_LEVEL` | `log.level` | `warn` / `info` / `debug` |

Per-tagger settings (enable, confidence, worker count) live in the
Settings UI, not env vars.

A few more environment variables affect runtime behaviour without
overriding a TOML key:

| Variable | Effect |
|---|---|
| `TZ` | Timezone name (e.g. `Etc/UTC`) for displayed timestamps and the daily scheduled maintenance. Defaults to `UTC` when unset. |
| `MONBOORU_TAGGER_BACKEND` | Set to `inproc` to disable the default subprocess tagger backend and run inference inside the parent process. |
| `MONBOORU_TAGGER_WORKER_LOG` | Log level for the `tagger-worker` subprocess only. Accepts the same `warn` / `info` / `debug` values as `MONBOORU_LOG_LEVEL`. Read once when the worker starts. |

ONNX Runtime and CUDA also honour their standard environment
variables:

| Variable | Effect |
|---|---|
| `ORT_LIB_PATH` | Absolute path to `libonnxruntime.so` when the shared library is not on `LD_LIBRARY_PATH` / `/usr/lib`. Only consulted by the `-tags tagger` build. The Docker image bundles ORT and does not need this. |
| `CUDA_CACHE_PATH` | Directory for the cuDNN JIT cache. When `tagger.use_cuda = true`, monbooru defaults this to `<data_path>/.nv-cache/` so the cache survives container recycles. |

## Log levels

`log.level`:

- `warn` (default) - warnings, errors, and failed / rate-limited logins.
- `info` - adds one line per non-noisy HTTP request, startup banners, and
  explicit mutations (successful logins, settings changes).
- `debug` - adds static asset, thumbnail, `/health`,
  `/internal/job/status`, and `/internal/monloader-status` hits.
