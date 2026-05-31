# Auto-tagger

The auto-tagger uses ONNX models to suggest tags for your images. Off
by default.

## Install a supported tagger from the catalog

1. Open **Settings → Auto-Tagger**. The table lists available taggers
   (WD14 SwinV2, JoyTag, Camie v2).
2. Click "Show instructions" on the row you want. The dialog has shell
   snippets for both host install and `docker exec` install.
3. Run the snippet on a machine with internet access. 
4. Refresh the settings page and tick `Enabled` on the row.

## Install a custom ONNX model

Other custom ONNX models may or may not work. Drop the model into its
own subfolder under the `models/` volume. Each subfolder needs:

- `model.onnx` - the weights.
- One label file: `tags.csv` (WD14 schema: `tag_id,name,category_id`),
  `tags.txt` (one label per line, all `general`), or a Camie-style
  metadata `.json` (`dataset_info.tag_mapping.idx_to_tag` +
  `tag_to_category`).

Reload the Settings page; the new tagger appears in the table.

To run it, use the auto-tag button in the image detail or in batch
actions.

Multiple taggers can run together; per-image results are merged so a
tag detected by two taggers is inserted once with the higher
confidence.

## Thresholds and per-category caps

Each tagger has a global confidence threshold plus an optional
per-category override map. Open **Settings → Auto-Tagger → Configure**
on a row to edit the global threshold, add overrides for individual
categories (e.g. raise `character` to 0.85 to suppress false-positive
character tags while keeping `general` permissive), and set a
**Max tags** per category - the maximum number of tags this tagger may
emit for that category on one image after thresholding. Empty Max tags
cells fall back to the built-in defaults (`character` 8, `copyright`
4, `artist` 4, `general` 25, `rating` 1, anything else 10); `0` keeps
every tag that survives the threshold. Empty per-category threshold
cells fall back to the global threshold.

Every category is enabled by default. Untick **Enable** on a category to
mute it entirely: the tagger emits no tags for that category regardless of
score, so you can run a tagger for only the categories you want (e.g.
disable everything except `medium` and `meta`).

## Frame-merge gate (videos and archive only)

When a tagger runs against a video (5 sampled frames) or an archive
(every page), monbooru merges per-frame scores into a single set of
tags per image. The `tagger.aggregation.min_hit_fraction` TOML knob
(default `0.05`) controls how many frames a label must score above the
threshold on to survive the merge: the cutoff is
`clamp(ceil(min_hit_fraction × frame_count), 2, 10)`. A single noisy
hit on a 200-page manga is not enough; the same label appearing on 10+
pages does survive. Set the knob to `0` to revert to "any single hit
wins". Static images are unaffected (always single-frame).

## Per-gallery enabling

Each tagger row has a Galleries column with a Configure button. Tick
"All galleries" so the tagger fires on every gallery (default). Tick
individual galleries to restrict it to just those - useful when one
gallery holds anime work and another holds photos and you don't want
WD14 firing on the photos.

## Override label routing (dispatcher)

Drop a `dispatch.json` next to the tagger's `model.onnx` to remap a
label to another category, rename it, or drop it entirely. The shipped
defaults are at `internal/tagger/dispatch_default/<tagger>.json`.

Schema:

```json
{
  "version": 1,
  "rules": [
    { "source": "monochrome",      "category": "medium" },
    { "source": "artist_name",     "category": "meta"   },
    { "source": "ugly_label",      "category": ""       },
    { "source": "twitter_username","category": "meta", "name": "twitter" }
  ]
}
```

- `source` matches the raw label the model emits.
- `category` is the destination category name. An empty string drops
  the label entirely.
- `name` (optional) renames the tag on insertion; empty keeps the
  source name. The renamed value is run through the tag-name allowlist
  before storage.

The overlay applies on top of the embedded default for the same
tagger: same-source entries replace the default, new sources append.
Rules pointing at a category that does not exist on the gallery are
skipped with a debug log; the embedded default for that source
survives the failed override.

## GPU (CUDA)

The default image is CPU-only (~210 MB). For GPU inference, switch to
the `-cuda` image (~2.3 GB), pass the GPU into the container the usual
way, then enable **Settings → Auto-Tagger → Use GPU (CUDA)** (or set
`MONBOORU_TAGGER_USE_CUDA=true`). GPU makes batch auto-tagging a lot faster.
The current mode is shown as a badge. 
Worker count is configurable from **Settings → Auto-Tagger** or
`tagger.parallel` in TOML (default 4); raise it on GPU if preprocessing
becomes the bottleneck.

The very first GPU inference on a new host pays a one-time
JIT-compilation cost, which takes a few minutes during the first inference. The
compiled kernels are cached under `<data_path>/.nv-cache/`; every
restart after that loads them in ~2 s. The cache can be set explicitly
with the standard `CUDA_CACHE_PATH` env var if you want it elsewhere.
Mount the data path on a persistent volume so the cache survives
container recycles.

## Idle release

The model stays loaded for 15 minutes after the last run, then unloads
to free memory. Tune via **Settings → Auto-Tagger → Tagger RAM/VRAM
idle release (minutes)** or `tagger.idle_release_after_minutes` in
TOML; `0` releases immediately after every run.

By default the tagger runs in a forked subprocess (`tagger-worker`)
that the parent supervises - idle release SIGTERMs the child so the
kernel reclaims the CUDA libraries and the ONNX Runtime arena. The
parent's RSS stays at the no-tagger baseline between runs regardless
of how long the model stayed loaded. To run inference in the parent
instead, set `MONBOORU_TAGGER_BACKEND=inproc` before launch.
