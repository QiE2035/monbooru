# Changelog

## [v1.7.1] - 2026-05-14
### Added
- API: `GET /api/v1/images/{id}/tags` returns the per-image tag list with the same shape as the POST/DELETE counterparts.
- Search: `*xyz` parses as a suffix wildcard; `collection:` values autocomplete.
- Tags page: alias rows accept a category change; the create-alias dialog auto-creates a missing canonical target.
- Tag and alias mutations surface their result in the tag-flash slot.
- Detail page: topbar reshuffled; the inbox button labels by current state.
- Batch: selection bar and Actions chooser share one ordering; new `Toggle favorite` action; `Send to inbox` / `Remove from inbox` collapse into `Toggle inbox`.

### Changed
- Logger levels reorder `Debug < Info < Warn` to match slog; runtime semantics unchanged.

### Fixed
- Auto-tagger: pad-and-resize peak allocation capped to `size^2` so large sources can't OOM-kill the container with multiple taggers loaded.
- Search: `tagged:true` / `autotagged:true` partition counts cached and invalidated alongside image-tag membership writes; `inbox:` shapes pin `idx_images_ingested_visible` to stay inside the search budget.
- Search: bare `system:` returns no-match instead of matching every image; random sort with a small seed actually shuffles.
- Reader and gallery: out-of-range `?page=N`, `?page=0`, and `?page=-1` 303 to the clamped value so the URL agrees with what the pager renders.
- Detail page: clearing the collection nulls the stored order; pasting many tags commits in a single transaction.
- Tags page: duplicate category names surface as a friendly error; zero-usage rows skip the origin badge; alias canonical-suggest dropdown anchors to the input width.
- Categories: new-category color picker aligns with the text and Add controls.
- API: `tags` is required on POST/DELETE image-tag mutations; `page_size` aliases `limit`; `via` rejects selector-breaking characters; cbz tag mutation drops its leftover 415; path-mode ingest enforces gallery-root containment and rejects non-decodable files; delete cleans up an emptied source folder by default.
- Maintenance: prune-missing runs as a job, blocking concurrent submissions.
- Gallery: alias path collisions caught before the move transaction; watcher suppresses events during delete and tag jobs; thumb checkbox blurs on change so keyboard shortcuts fire after a click; bulk delete skips `RemoveMangaCache` for non-cbz rows.
- Tagger service: rating-category lookup miss is logged instead of silently producing a Service with `ratingCatID=0`.
- Router: `custom_css` containment check resolves symlinks first so an operator can't accidentally point it at a symlinked escape.
- Scheduler: cancel aborts the whole run instead of just the current phase.
- UI: toolbar tooltip keeps the inbox count visible when a rating ceiling is applied.
- Batch tag: chunk size drops to 100 rows when at least one resolved tag has implications so foreground requests slip between commits.

### Internal
- DB bootstrap: ANALYZE gated on `PRAGMA user_version`; partial-index ANALYZE bounded with `analysis_limit = 400`. Steady-state cold starts skip the pass.
- `internal/web/handlers_batch.go` extracts `resolveBatchScope`; `gallery.Sync` splits into per-case helpers; `galleryCtx` cached-count accessors share one helper; favorite/inbox toggle handlers share `toggleBoolColumn`.

## [v1.7.0] - 2026-05-11
### Added
- Comics & manga. `.cbz` / `.zip` archives ingest as single entries with `page_count`, optional `Collection` and order, and a parsed `ComicInfo.xml` panel. In app reader (`/images/{id}/read?page=N`) with bottom-bar controls and side click-to-prev/next chevrons; pages grid (`/images/{id}/pages`) with keyboard nav. Auto-tagger iterates every archive page. Search keywords `type:image` / `type:archive` / `pages:<op><n>`. Detail-page `Open in reader (R)` and `See all pages (P)` buttons. Manga `N pages` pill on gallery thumbnails.
- Collections. Any image can carry a `Collection` label and an optional order integer (rendered `#N`). Surfaces: detail-page edit dialog with autocomplete, search keyword `collection:<value>`, sidebar Collections section above Folders, batch `Set collection` action, `Order` sort (collection then order), thumbnail pill bottom-left.
- Search keyword `type:animated` replaces `animated:true` / `animated:false`. The `type:` vocabulary unions `image`, `archive`, and `animated`; static-only is reachable as `-type:animated`.
- Detail-page e-leader chord shortcuts: `e o` order, `e s` source, `e c` collection, `e u` url.
- Auto-tagger: frequency-gated merge with a per-category top-K cap (new `tagger.aggregation.min_hit_fraction` knob).  Manually re-adding an auto-tagged tag promotes the row to user-owned. Cancellation now responds inside the manga page loop, and status emission is serialised across parallel workers so progress reads as a stable side-by-side list.
- Upload: live progress feedback (`Uploading… <loaded> / <total> (NN%)`) and a `Saving on server…` label once bytes are fully sent.
- Detail page: orange warn-flash on mixed-outcome tag adds (some accepted, some rejected) bundling every applicable part; inline flash when a manual rating overwrites another.
- Settings: confirm dialog before regenerating the API token.
- Gallery grid: `ArrowDown` on the last visible row scrolls the layout to its bottom (and `ArrowUp` on the first to its top) so the search bar and pagination stay keyboard-reachable.

### Changed
- Detail-page metadata rows reorder to `Added via` / `Source` / `URL` / `Collection` / `Order` / `Pages`. `[edit]` markers render as inline text links. Duplicate-paths section relabelled `Duplicates` and restructured as a vertical list. `Similar images` renamed to `Similar entries` and partitioned by file type.
- Sidebar heading `AI source` shortened to `AI`; Collections section moves above Folders.
- Maintenance: `VACUUM` runs in a goroutine instead of on the request thread.
- Gallery export JSON bumped to v2 (adds `manga_metadata` array); v1 imports stay supported. Light-manifest version split from the full-export version so it stays at `1`.
- `tagged:true` / `autotagged:true` exact partition counts compute `visible_total - untagged_visible` so the two halves sum to the visible total.

### Fixed
- Sync: in-place edits (file rewritten at same byte length) detected via a new `image_paths.mtime` column; sha, dimensions, side-table metadata, and thumbnail refresh in place so user-curated tags survive.
- Ingest / sync: when a known SHA reappears at a new path, the previous canonical row is demoted to alias instead of being silently rewritten in place.
- Watcher: re-debounces on `Write` events so slow `.cbz` copies aren't ingested mid-flush.
- Search: saved searches persist `sort` / `order` / `seed`; `View inbox` link on the upload page shows a ceiling-aware count; bare-category-prefix filter (`?q=character:`) routes to the category-only filter; pages clamped past the end issue `303` (or `HX-Push-Url`) so the URL matches what the user sees.
- Gallery grid: overlay badges (favourite, inbox, missing) swap unicode for inline SVG so glyphs align across fonts; manga `N pages` pill anchors bottom-right of `.thumb-card`.
- Settings: active-gallery row counts pinned to the `baseData` snapshot so footer and table cells agree; password verification gates on the stored hash, not the `EnablePassword` flag; password-set form gets a hidden `username` field for password managers and accessibility tools.
- Maintenance: alias / duplicate-path deletes route through `gallery.PathInside` so they can't unlink files outside the gallery root; direct hits on `/settings/maintenance/duplicates-list` 303 to `/settings#maintenance`.
- Server: `custom_css` path validated against a trusted-dir allowlist at config load; `/favicon.ico` aliases to `/static/favicon.png`.
- Layout: footer wraps on viewports under 600 px; switch-gallery dialog closes on the success branch.
- Jobs: `Manager.Cancel` releases `scheduleHeld` so an early-return doesn't leak the reservation; `prune-thumbs` joins the cancel-button gate.
- Scheduler: cache invalidation moved into a `galleryCtx.Sync` wrapper so manual and scheduled syncs both refresh per-cx tag / folder / source caches.

### Internal
- `internal/web/handlers.go` split into 15 per-surface files. No behaviour change.
- Schema bootstrap (additive): `images.page_count`, `images.series`, `images.series_order`, `image_paths.mtime`, `saved_searches.sort` / `order` / `seed`. New `manga_metadata` table; new `idx_images_series` partial index.
- Per-page thumbnails pre-generated at ingest so the first `/pages` render is a static-file serve. 
- Batch tag operations chunk the per-tag transaction (one tx per 500 rows), mirroring `runBulkDelete`.
- Tests added: auto-tagger `storeResults` scope + rating prune, archive path-traversal, implication cycle / self-edge / depth-bound, user / auto tag-removal scope, bulk-delete cascade, vacuum handler, image-serve path-traversal, EXIF UserComment fixtures.
- Docker / Podman image refresh.

## [v1.6.0] - 2026-05-09
### Added
- Inbox/archive triage. Newly ingested or uploaded images land in the inbox (`is_inbox=1`); the user flips them to archived once curated. Pre-existing libraries upgrade as fully archived (the migration backfills `is_inbox=0` so nothing dumps into the inbox view on first boot). Surfaces: `inbox:true` / `inbox:false` search keywords, a toolbar `Inbox` toggle with a live count, a thumbnail grid badge, a detail-page button (`i` shortcut), and a single Send-to-inbox / Archive batch toggle on the selection bar and the Actions chooser.
- Upload page: inbox backlog count rendered next to a `View inbox` link.
- Favourite styling switched from the yellow ★ glyph to a red heart (♥/♡) across the gallery grid overlay, the toolbar `Favourites` filter button, and the detail-page action row. 
- Per-image external metadata: new `images.source` (free-form provenance label, e.g. site name) and `images.url` (canonical web URL) columns, edited from the detail page via small `[edit]` pop-ins. The URL field is rendered as an `http(s)`-only target=_blank link.
- Search keyword `source:my_label` for exact-match against the new `images.source` field, riding `idx_images_source`.
- Keyboard shortcut overhault:
  - Keyboard shortcut overlay: press `?` anywhere to display the cheat-sheet.
  - Chord-based navigation: `g g`, `g u`, `g c`, `g t`, `g s`, `g h` jump to gallery / upload / categories / tags / settings / help.
  - Vim aliases: `h j k l` for the gallery grid cursor, `[` and `]` for prev/next page, `g g` / `G` for first/last page, `p` for the page-jump dialog. Detail-page `h l j k` alias the prev/next-image arrows.
  - Gallery view-level shortcuts: `F` / `I` toggle the favorites / inbox filters, `R` random-sorts, `O` cycles sort, `D` flips direction, `S` opens save-search directly, `Home` / `End` jump to the first / last visible thumbnail.
  - Selection-bar shortcuts: `m` moves, `i` toggles inbox/archive, `Delete` deletes (with confirm), in addition to the existing `a` / `r` / `t`.
  - Detail page shortcuts: `m` opens the move dialog, `o` opens the original in a new tab, `Backspace` returns to the gallery.
  - Tags page shortcuts: `n` opens the create-tag dialog, `N` opens the create-alias dialog, page navigation parity with the gallery.
  - Categories page: `n` focuses the Add category form. Settings page: `1`-`6` jump to the section headings.
  - Anywhere: `Y` clicks the topbar Sync button, `,` / `.` walk the SFW ceiling, `\` opens the gallery-switch dialog when more than one gallery is configured.
- Settings → Stats section: per-gallery DB size and a memory containment tree.
- Footer: server-side render time displayed.

### Changed
- Detail-page metadata row labelled `Source` (showing the ingest origin) renamed to `Added via`, freeing the `Source` row for the new operator-edited label.
- Sidebar heading `Source` renamed to `AI source` to disambiguate from the new free-form source field.
- Search keyword `source:` renamed to `ai:` for the AI-generation-tool filter family. The omnibus value `source:ai` becomes `ai:any`. The other values (`a1111`, `comfyui`, `none`, `sd`) are unchanged. Saved searches and bookmarks must be updated by hand; there is no auto-rewrite.
- REST API field `source` on `POST /api/v1/images` and `POST /api/v1/images/{id}/tags` renamed to `via` to match the per-image `Added via` semantics.
- Tags-page row button `Merge→` relabelled `Alias→` (and its dialog prompt to `Alias <name> → to:` with submit text `Alias`).
- Detail page: implied tags and the alias section under the image fold into one collapsed `<details>` (closed by default). Summary text is `Implied tags`, `Aliases`, or `Implied tags and aliases` depending on what is present. Alias chips drop the merge-arrow + canonical span.
- Manual rating-tag adds now overwrite any existing rating on the image; the auto-tagger continues to keep the highest-rank rating it predicted.
- `GET /api/v1/tags` defaults to `show_zero=1` to match the /tags page; pass `show_zero=0` to hide declared-but-unused tags.
- /tags page row controls: Rename / Alias / Delete are hidden on the four canonical rating rows (they're reserved); the Category cell on rating rows and on alias rows is rendered read-only. Delete on a rating row still works as a usage-strip.

### Fixed
- /tags page: filter and pagination state preserved across rename, alias create, and alias/repoint merge so the operator stays on the same view after a write.
- /tags page: past-the-end page numbers clamp to the last populated page instead of returning empty.
- /tags page: tokens that match a category prefix only (`character:`) surface the category-only filter instead of being silently dropped.
- Detail page: prev/next layout no longer jumps when one neighbour is missing.
- Detail page: tag-focus mode survives the post-delete htmx swap; the cursor re-anchors to the next tag in the row.
- Detail page: partial-duplicate tag adds surface the duplicate token instead of silently dropping it.
- Gallery / detail: download, favourite, inbox, and external buttons share the same height across the action row; the favourite (♥/♡) glyph swap no longer reflows the row.
- Gallery: thumbnail badges share identical pixel dimensions (square, centred) so a card lines up with its peers.
- Search: gallery adjacency cache is invalidated on every membership write (tag add/remove, batch ops, alias merges) and on every `InvalidateCaches` call, so navigation no longer surfaces stale neighbours.
- Search: rating-ceiling chain is dropped from the loose-bound when the user expression has no upper bound, so cookie-applied SFW chains stop adding redundant work; ceiling+search COUNT defers to the slow exact COUNT instead of an inaccurate fast estimate.
- Search: `RankInQuery` fires on random sort under the existing gates (was over-conservative) and the cold-path worst case is bounded so a popular-tag detail page no longer walks an unbounded carrier set.
- Detail back link: page number aligns with the current image when the adjacency cache is warm.
- Scheduler: orphan-thumbnail sweep takes a job slot and observes context cancellation.
- API: `addImageTags` / `removeImageTags` pre-check image existence and return 404 instead of a write error; `POST /tagger/{name}` validates the tagger name from the URL against an allowlist; OpenAPI spec updated for the `source` / `via` / `url` rename.

### Removed
- BREAKING: search keyword `source:` (the AI filter form) is gone; use `ai:` instead. `source:` now means exact-match against the new `images.source` field.
- BREAKING: REST API field `source` is removed entirely from the two endpoints above. Use `via`. No deprecation period.
- BREAKING: gallery `h` / `l` page-navigation shortcuts; use `[` / `]` instead. The vim letters now alias the grid cursor.
- Keyboard shortcuts `PgUp` / `PgDn` (use `[` / `]`), `c` (use `g c`), `g r` (use `R`), and the detail-page `d` shortcut are dropped to match the new chord scheme.

### Internal
- New `idx_images_source(source)` and renamed `idx_images_source_type(source_type)` (the previous `idx_images_source` index was on `source_type`).
- `fastCountSource` helper renamed to `fastCountAI`; the slow-path `buildFilterExpr` flips `case "source":` to `case "ai":` and adds a separate `source:` exact-match branch.
- Search adjacency cache: gallery match-id list cached for adjacency reuse and reused by the gallery handler for multi-page searches; cap raised to 20 000 IDs; populated asynchronously through a singleflight gate.
- Search: covering index on `image_tags(tag_id, image_id)`; recent-id bound on multi-leg INTERSECT for newest sort (skipped when `order=asc`); multi-leg AND-driver INTERSECT capped at the two least-popular legs; AND-driver skipped in adjacency when the bucket gate fires; adjacency bucket gate skipped when the candidate set is small.
- Search: `suggestContextCap` tightened to 1 000 to bound the with-context autocomplete query under contention.
- DB: SQLite page cache capped at 2 MiB per connection; per-conn cache trimmed and idle write-pool conns closed after a quiet period.
- Tags: `ChunkedDeleteWithTagRecalc` extracted and folded into three callers; `suggestUsageRanked` extracted, replacing `search.suggestByUsage`; `mergeTagsPost` routes the canonical input through `resolveCanonicalTag` so alias / category resolution lines up across the tag-mutation surface.
- Maintenance: `prune-orphaned-thumbnails` runs as a background job through the scheduler instead of inline.

## [v1.5.1] - 2026-05-05
### Added
- Camie v2 auto-tagger as a third catalog entry (`camie-v2`). 789 MB ONNX, 70k+ tags across 7 categories. It's currently the only supported model that natively predicts artist (~7k tags) and copyright (~5k tags). 
- Per-gallery enabling for auto-taggers. Each tagger gains an optional `galleries = [...]` TOML key; empty / missing means every gallery (the legacy default), a non-empty list restricts the tagger to the named subset. Managed via Settings → Auto-Tagger → Galleries; Per-job entry points (gallery, detail page, upload, scheduler, REST API) all honour the per-gallery filter. 
- Newly discovered tagger model folders are persisted as enabled instead of staying implicit; the operator can disable from the table afterwards.
- Gallery batch-mode click toggle: while the batch bar is up, a plain left-click on a thumbnail toggles its checkbox.
- Manual and auto-tagger rating-tag adds prune lower-rank rating rows from the same image inside the write transaction. Pre-existing multi-rating rows from older data are left alone; only writes that fire the rule clean up.

### Changed
- Auto-tagger thresholds are now per-category: each tagger has a global threshold plus an optional `category_thresholds` map. Catalog rows ship with sensible defaults (wd-swinv2 carries `character = 0.85`; joytag stays single-knob since its profile only emits `general`). Edit per-tagger via Settings → Auto-Tagger → Configure;
- Settings → Auto-Tagger table redesigned: Downloaded and Enabled fold into a single Status column; the Confidence number-input is replaced by a Configure button + inline summary; a new Galleries column follows the same pattern; column widths pinned so rows align across taggers.
- Catalog default thresholds rebased according to models suggestions.

### Removed
- Detail-page X/Y position counter. The counter sat next to the back link and required a SUM/COUNT walk over the visible set on every detail-page load whenever the back-search carried no tag-shaped predicate to short-circuit on, affecting performance on large galleries. Adjacent prev/next navigation is unaffected.

### Internal
- Auto-tagger inference is profile-driven. Lifted the WD14-vs-JoyTag bool to a Profile struct capturing input size, layout, channels, normalize, pad, activation, label format, category scheme, and output index. Profiles resolve through embedded defaults at `internal/tagger/profile_default/<name>.json`, an optional `<modelPath>/<name>/tagger.json` sidecar, then a heuristic from the label-file extension. Profile fingerprint joins the cache reuse signature so a sidecar edit invalidates the session set.
- Auto-tagger label loaders gained a third format: `camie_json` parses Camie's `dataset_info.tag_mapping.idx_to_tag` + `tag_to_category` shape, populating `tagLabel.categoryName` for the `name_string` category-resolution path.
- Search: AND-driver materialises wildcard canonicals, so `blue*` at root substitutes a literal IN(...) instead of a per-row LIST SUBQUERY scan; popular AND-of-tags chains (every leaf above the rare-tag threshold) intersect off `idx_image_tags_tag` rather than nesting EXISTS over a full visible scan; a single-leaf AND-driver is now allowed under random sort.
- Search: rating-ceiling chain peels off `AndExpr{userExpr, chain}` so the count fast path applies to rating-filtered user searches, with a sum-of-usage upper bound on the chain leg; fast counts added for `rating:`, `folder:` recursive, `source:` csv, `tagged:` / `autotagged:`, and `generated:` filters; `idx_images_ingested_visible` pinned for the `tagged:` / `autotagged:` data path.
- Search: id-bucket gate for newest-sort sparse multi-AND prev/next adjacency.

## [v1.5.0] - 2026-05-02
### Added
- Tag implications: declare `parent → child` so adding the parent stamps the child on every image carrying the parent, and removing the parent walks the transitive closure to strip children that were only justified by it. Declaring or deleting an implication runs a chunked background propagation job. 
- /tags page rework: create-tag button and dialog; ascending/descending order filter; direct alias CRUD with category change; zero-usage filter (`Show` / `Only` / `Hide`) for declared-but-unused triage; danger button to delete every tag in the current search.
- Built-in `rating` tag category with canonical tags `general`, `sensitive`, `questionable`, `explicit`. The category is reserved, locked (no rename / merge / delete / move-category), and accepts only the four canonical names. Search supports `rating:explicit` with highest-wins semantics: an image carrying multiple ratings matches only the highest. The detail-page sidebar lifts the rating group to the top.
- Footer rating-ceiling selector. The footer cluster `rating: sfw . sensitive . questionable . explicit` posts to `/internal/rating-ceiling` and writes the `monbooru_rating_ceiling` cookie; gallery, sidebar, related-images, and detail-page adjacency then hide images carrying any rating above the ceiling. The first label `sfw` is a display alias for `general`. Folder tree, source counts, and the cached visible-count deliberately ignore the ceiling.
- Built-in tag categories `medium`, `person`, and `year`, so Hydrus-style `medium:digital`, `person:alice`, `year:2026` tokens round-trip into typed categories instead of landing as literal `general` tokens. Pre-existing user-created custom categories with these names are promoted to built-in on the next bootstrap. Hydrus `creator:` is rewritten to `artist:`, and `series:` / `studio:` to `copyright:`, on sidecar import.
- Operator-supplied `custom.css` override loaded after the built-in stylesheet, so a self-hoster can tweak the look without forking the binary.
- Gallery Actions chooser collapses Tag all / Remove tags / Auto-tag / Move / Delete behind one button, with arrow-key focus cycling, sub-dialogs (Remove tags strip, Auto-tag), and Escape / Cancel popping back to the chooser.
- Batch operations on the current search: auto-tag all, and move all to a folder. 
- Gallery batch-selection keyboard shortcuts: Space toggles, `a` adds tags, `r` removes tags, `t` opens auto-tag, `Ctrl+A` selects all, `Esc` clears.
- Detail page: tag-focus mode (`r` enters, arrows cycle on-image tags, Enter removes); a dim Aliases group lists aliases pointing at on-image tags; `t` opens the per-image Auto-tag dialog.
- Search: `system:` cheat-sheet autocomplete covers filter keywords and category drill-ins, with a description column on cheat-sheet rows.

### Changed
- Auto-tagger: WD14 rating labels (`general`, `sensitive`, `questionable`, `explicit`, plus the `rating:*` variants) now route to the `rating` category instead of `meta`. Pre-existing libraries with `meta:general`/`meta:sensitive`/etc. tags are not migrated automatically; re-run the tagger or move tags manually to switch them over.
- Zero-usage tags are no longer auto-pruned. Use the /tags Zero-usage filter for manual cleanup.
- Delete on a rating tag strips its usage from every image but keeps the row, since the four canonical ratings are reserved.

### Fixed
- Tag implications integrity: `MergeTags` repoints `tag_implications` to the canonical and promotes the canonical row when the alias was user-owned but the canonical was implied; `DeleteTag` sweeps the implied closure on every carrier image before dropping; `CreateAlias` repoints `tag_implications` when upgrading or repointing an existing row.
- Watcher: marking a file missing decrements `usage_count` of every tag the image carried and prunes any that hit zero, in the same write transaction as the is_missing flip. Tag counts no longer drift between watcher events and the next manual recompute. Alias paths are ignored in the mark-missing fallback, and the alias is promoted to canonical when the canonical's old path is gone.
- Search: `date:` filter accepts `YYYY` and `YYYY-MM` and is now validated; bare wildcard tokens become no-match instead of a full scan; random-sort seed is clamped to 31 bits.
- Saved searches: duplicate names are rejected.
- Import: type-to-confirm is validated before the upload is buffered; imported DB is bootstrapped before sanitisers run; per-entry decompressed size on zip archives is capped; light merge skips the wipe when the archive carries no files.
- Config: non-bcrypt `password_hash` is rejected on startup; non-positive `SessionLifetimeDays` is clamped to the default; `MaxFileSizeMB <= 0` is treated as no per-file cap.
- Auth: same-origin Referer check rejects non-http(s) schemes.
- API: trailing slash on `base_url` is trimmed before the CORS check.
- Footer: cached counts are invalidated on tag and saved-search mutations.

### Removed
- Daily schedule: the Recompute tag counts and Vacuum database toggles. Vacuum stays available as a manual button under Settings → Maintenance. Existing `recompute_tags` / `vacuum_db` keys in `monbooru.toml` are ignored on load and dropped on the next save.
- Settings → Tag actions: Run untagged / Run all (rolled into the gallery Actions chooser).
- `MONBOORU_UI_PAGE_SIZE` env override.

### Internal
- Search: multi-AND drives off the smallest tag's `image_tags` rows; `rating:` filter short-circuits when no image carries the level; suggest candidate cap halved to lower with-context contention; partial index for alias rows on `canonical_tag_id`.
- Tags: `MergeTags` folded into three set-based statements; `DeleteTag` bulk-deletes instead of per-image looping.
- Auto-tagger: per-model label dispatch tables embedded in the binary.
- Single source of truth for filter-keyword vocabulary across search and autocomplete.

## [v1.4.3] - 2026-04-29
### Fixed
- Auto-tagger: ORT environment and per-tagger sessions are cached across runs so back-to-back jobs no longer leak ~440 MB of glibc-arena memory each. The cache is torn down after `tagger.idle_release_after_minutes` (default 30) of inactivity, on Settings save, and on `use_cuda` flips.
- Auto-tagger: completion summary now reports `auto-tagged X of N image(s), K skipped` when images are skipped (missing rows, no extractable frames, store failures), instead of always reporting the submit count.

### Internal
- Search: prev/next adjacency on the detail page uses a row-value cursor and pins the partial sort index, dropping cursor lookups from seconds to milliseconds on large libraries; random-sort + tag-predicate adjacency bounds the outer scan to a 2000-id bucket around the current image; random-sort wildcard searches materialize the EXISTS body as an IN-list instead of re-evaluating per row.
- Search: detail-page X/Y counter is skipped for tag-predicate and folder-filtered searches; cursor-based prev/next still works and the template hides the counter when the total is zero.
- Memory: read connections run `PRAGMA shrink_memory` and `runtime/debug.FreeOSMemory()` every 5 minutes when the job manager is idle, so RSS plateaus instead of holding the post-burst peak.

## [v1.4.2] - 2026-04-28

### Internal
- Search: fast-path counts for pure-tag, wildcard, NOT, AND, OR, and category-qualified queries; pinned partial sort index for the unfiltered-newest path; fast-path approximations gated on a per-query usage threshold.
- Search: autocomplete candidate and context caps tightened; context CTE capped at 50k rows.
- Tags: `RelatedImages` drops popular tags from the seed set and uses a smaller candidate buffer; tag-count recalc chunks by `tag_id` range to release the SQLite writer.

## [v1.4.1] - 2026-04-27

### Added
- Tag names accept `?`, `>`, `<`, `=`, `^`.
- Enabled auto-taggers whose model files have gone missing are auto-disabled at startup instead of failing silently at inference time.
- Settings → Auto-Tagger: per-tagger download instructions. 
- Detail page: "Remove all tags" confirms before running.
- Auto-tag picker on the detail and upload pages is a radio list rather than a dropdown.

### Changed
- Schedule defaults: `run_auto_taggers` is `false` so freshly-installed instances don't auto-run taggers on every cycle; 
- Gallery import dialog defaults to Merge.
- Settings → General reshuffled into side-by-side rows: Files / UI, GPU / Parallel workers, Password / API token; Schedule checks render as a 2x3 grid; tags filters are clickable buttons rather than dropdowns; the tags-page filter submit is inline with a shorter label; auto-tagger actions are grouped under "Tag actions"; copy in Tag actions and Auto-Tagger sections is tightened.
- Galleries table: Info column renamed to Content and the saved-count dropped; per-action columns; actions right-aligned; Add gallery moved into a dialog opened by a button below the table.
- Auto-Tagger table: Disable column renamed to Enable; Downloaded moved left of Enabled; Status column width reserved so active and default badges stay side by side; tagger name column widened, status columns shrunk; bulk auto-tagging columns right-aligned and compacted.
- Detail page: columns stack on narrow viewports; "Tag all" is hidden while thumbnails are selected; a separator sits between Tag selected and Move selected.
- Upload UI: drop zone enlarged with bottom fields split into two columns; drop-zone prompt centered; reset button subdued.
- Built-in category Action cells expose a labelled Delete button instead of a generic icon.
- Dialog placeholders and microcopy refreshed across the UI; the batch-tag dialog placeholder spells out Add/remove.

### Fixed
- Auto-Tagger: emoticon-only labels (`:3`, `:o`-style) survive `sanitizeLabel` instead of being dropped; `_unsupported_<idx>` placeholder labels are filtered at inference time.
- Recalc and merge-general-tags invalidate the affected caches so stale counts don't survive a tag rewrite.
- Search-suggest hover is suppressed until the mouse moves, so the dropdown no longer lights up under a stationary cursor.
- Upload action buttons stay fixed when the auto-tag picker reveals, instead of shifting.
- Add-gallery dialog fields stack on their own lines so long paths don't overflow.
- Settings action headers align with the buttons in their column; settings tables wrap in a scroll container so they stay readable on narrow viewports.

### Internal
- Performance: per-gallery distinct-tagger and tag/saved-count caches; `SuggestTagsWithFilter` materialises its filter context once and caps candidate tags before combo counting; partial index pinned for the unfiltered-newest sort; partial index on `is_missing=0`; index on `image_tags(tagger_name)` for `is_auto=1` distinct scans; lazy sidebar URLs carry page IDs; the `DiscoverTaggers` walk is shared between settings and bulk-tagger rows.

## [v1.4.0] - 2026-04-26

### Added
- Import hydrus network and bloombooru exports directly from the gallery import dialog. (images and tags)
- Add gallery form accepts an optional `.db`/`.json`/`.zip` upload; the new gallery is created and the file imported into it in the same step. Add and merge import now switch the active gallery automatically.
- Settings → Auto-Tagger lists a catalog of suggested taggers (WD14 SwinV2, JoyTag) with copy-paste install commands relative to the host working directory; `<model_path>/models.json` overrides extend the catalog. Disabled tagger rows expose a Delete button.
- Batch tag add/remove from search and from selection: a Tag all button next to Delete all in the gallery header and a Tag selected button in the batch bar share a dialog with autocomplete and an Add/Remove radio.
- Bulk tag removal moved to its own Settings → Tags section so it stays reachable when no on-disk tagger is configured.
- Settings → General is one form with a single Save.
- Styled 404 page with a back link instead of bare text.
- Built-in category Action cells are labelled `(built-in)`.

### Changed
- Detail image floored at 200 px so tiny files no longer render as a dot.
- Upload list shows `<1 KiB` for sub-1 KB files instead of `0 KiB`.
- The `?` thumbnail fallback explains via a hover title why the thumbnail is unavailable.
- Gallery status reads "N matches" instead of "match N".
- Download button uses `⤓` to disambiguate from the sort-direction `↓`.
- Gallery batch action bar reordered: Tag selected sits left of Delete selected; Move/Delete are right-aligned and separated from Tag selected.
- Add gallery form is single-row with equal-height controls, drops the verbose hint, shows a busy state on submit, and renders a success flash before the page reload. The gallery import dialog drops its redundant hint too.

### Fixed
- Search/UI: search input blurs on submit and the suggest dropdown is dismissed; stale suggest swaps no longer race the new query; long tag chip names ellipsize on the detail page; the detail breadcrumb back-link stays on one line when the filename is long; the gallery toolbar (Save / Tag all / Delete all) tracks the current search via OOB swap.
- Search: LIKE metacharacters in tag prefix/substring and folder filters are escaped; malformed queries return 400 instead of being silently parsed away.
- API: `tag_warnings` are returned from `addImageTags` and surfaced in the upload flash; compat-imported tags are credited to the originating provider instead of always `import`.
- Auth/audit: settings audit logs record the `clientIP` so reverse-proxy IPs are captured; login backoff is capped at 30 s as the spec documents.
- Web: `syncTrigger` redirects only to a same-origin Referer; export warns when a gallery file is skipped because its path is unreadable.
- Tags/gallery: errors propagate from `DeleteCategoryMoveOrDelete`'s delete-all path, `Sync`'s scan loop, and adjacent-image lookups instead of being dropped; orphaned-thumbnail prune bails on a partial id scan rather than deleting valid thumbnails.
- Config: `ui.page_size` is clamped to its default when a non-positive value is configured.

### Removed
- `tools/blombooru-export.py` : superseded by the in-app blombooru import.

### Internal
- Compat layer split into `internal/web/compatibility/` with per-app translators (blombooru, hydrus) self-registering against a package-level provider table. Adding a third format is a new file with one Register call.
- Performance: cache the general-category id on the gallery context so `parseTagInput` skips a SELECT; narrow `RelatedImages` to the only column the template uses; scope `RecalcAndPrune` to touched tags in bulk removals; add partial indexes on `generation_hash` for both metadata tables.
- Docs: new migration guide (`docs/MIGRATING.md`)

## [v1.3.0] - 2026-04-25

### Added
- Per-gallery import/export controls in Settings → Galleries. Imports can replace current gallery or merge with it. You can export/import only the database (as full .db or .json or as a minimal .json) or the database+images as .zip.
- Tag aliases: adding or searching a tag under any of its alias names resolves to the canonical tag. Auto-tagger output goes through the same alias resolution before it lands on a row.
- Gallery delete/replace dialog requires typing the gallery name before it confirms.
- Rebuild-thumbs is auto-queued after a gallery import.
- New `tools/blombooru-to-light.py` exporter that converts a blombooru install into a Monbooru light archive ready for the import flow.

### Changed
- Gallery thumbnail focus outline thickened so keyboard focus is easier to spot.
- Aliases now live in the main tags table instead of a separate one, so they share the tag pipeline end to end.

### Fixed
- Gallery: scan errors from Ingest's duplicate-path lookup surface to the caller instead of being swallowed. Re-extract per-image work runs in a transaction.
- Web: per-file size cap is enforced on uploads. Archive entries are checked for path containment with `filepath.Rel`, and the watcher uses the same approach for gallery-path containment. Removing a gallery folder refuses to follow a symlink.
- Search/UI: the favorites filter button stays active across composed queries. The login-rate-limiter shift is clamped to a non-negative range. `warmCaches` nil-deref race and dead `HX-Refresh` flashes resolved.
- Tags: errors from `RecalcAndPruneIDs` propagate and are logged at callers. `folderonly`, `tagged`, and `autotagged` are reserved as category names. Remaining `rows.Scan` and mark-missing loops surface their errors.
- Gallery video probe: ffmpeg invocations terminate option parsing with `--` before output paths so filenames with leading dashes can't be parsed as flags.
- Filter-keyword set is hoisted to a single canonical source shared between search and web.
- Docker Compose example image path on GitHub points at the right repository.

### Internal
- Cleanup pass on stale comments and dead notes; readme updates; assets recompressed.

## [v1.2.3] - 2026-04-21

### Added
- Status bar row under the gallery header and on the detail topbar.
- Tmux-style footer with gallery / tags / saved-search counts;
- Images: per-image `origin` recording how the file entered the gallery (`ingest` for watcher/sync pickups, `upload` for web and multipart-API uploads, or a caller-supplied string via a new `source` field on `POST /api/v1/images`). Surfaced on the detail metadata panel and in the API `Image` response.
- API: `POST /api/v1/images` and `POST /images/{id}/tags` accept a `source` field for manual tags. The detail page splits the user section into "Tags added by the user" plus one bucket per third-party source.
- Detail page action row: **Move image** and **Delete** are grouped together on the right of the Danger zone.
- Some UI style changes

### Changed
- Destructive buttons (delete, delete-all, delete-selected, remove-all) now render as solid red.
- Settings → Run Auto-Tagger: bulk run buttons relabeled to "Auto-tag all untagged images" / "Auto-tag all images"; the three Remove-all buttons move to a new "Bulk tag removal" subsection so destructive actions live apart from autotag-triggering ones.
- Detail page tag chips drop the "auto" badge; chip names are colored by category instead.

### Fixed
- Deleting an image reached through a Similar-images chain walks browser history one hop back (so Escape on `A → B → C` returns to `B`, then `A`) instead of pushing a fresh history entry and dropping the chain. Deleting the chain's source falls back to the referring search, then the gallery.
- Search and sort state is dropped when switching galleries, so the new gallery opens on a clean view instead of inheriting an irrelevant query.
- Detail-page panels and header align with the gallery frame.
- Detail-page tag chip names render in their category color.
- Excessive margin on the topbar sync button.

## [v1.2.2] - 2026-04-20

### Added
- Detail page: gallery-style search bar at the top of the page; submits as a plain GET `/` so the next view is a full gallery render with the chosen query, sort, and order. The input autocompletes against tags the same way the gallery input does.
- Detail page: folder, source, and saved-search sections appear in the sidebar below the image's tag groups, lazy-loaded so the image tags paint first.
- Detail page: position/total counter (e.g. `34/243`) renders between the back link and the filename when the page was opened from a search, computed from the same key-column comparison as the prev/next arrows.
- Detail page: deleting an image moves to the next image in the referring search (falling back to prev, then the gallery) instead of bouncing back to the grid.
- Detail page: videos autoplay muted with `playsinline`, and spacebar toggles play/pause (suppressed while typing in tag/search inputs).
- Similar-image navigation: clicking a related image carries a back-ref so Escape (and the "← Previous image" link) unwinds chains of any depth one hop at a time via the browser history. The gallery-context UI (X/Y counter, prev/next arrows, "← Images" back link) is hidden once you've switched images, since the current image isn't necessarily in the referring result set.
- Keyboard: `s` focuses whichever `#search-input` is on-screen on any page; `t` keeps focusing the tag input on the detail page, and `f` keeps toggling favorite.

### Fixed
- Related-images probe caps only the `general` bucket to the 15 rarest tags. Previously capping every non-meta category could flatten character/artist/copyright signal to the same 15-slot budget as the noisy general bucket; now those categories pass through uncapped while `1girl`-style tags no longer drag tens of thousands of rows into the candidate `GROUP BY`.
- Per-gallery sidebar caches (folder tree, source counts, visible count) pre-warm at startup instead of populating lazily in parallel on the first cold render.
- Sidebar searches skip the count pass; it was a second full filter evaluation for a number the handler never surfaced. A new partial index on `file_size` (visible rows only) turns sort-by-size over large libraries into an index lookup.
- Detail-page header controls (input, buttons, selects) share a single 28 px height and consistent padding, so buttons no longer render taller than the selects next to them.

## [v1.2.1] - 2026-04-19

### Fixed
- Tag names containing `:` (like `:3`) round-trip cleanly through the search parser, the auto-tagger, and the category-qualified API DELETE endpoint, without colliding with the `category:tag` syntax.
- Detail page: filename next to the back link and in the topbar/title; double disclosure marker on the metadata panel dropped; ComfyUI refs scroll to the referenced node; invalid-tag error clears while typing; search autocomplete no longer rewrites the URL.
- Dialogs: move-image dialog shows the current folder; move/delete-selected dialogs pluralize correctly; "1 image" no longer renders as "1 images"; merge-dialog autocomplete anchors below the input.
- Maintenance: destructive and long-running actions confirm before running and use action-named OK buttons.
- Settings: Schedule section shows last/next run; the two General Save buttons are disambiguated; gallery status renders as two distinct badges; login form is disabled when password auth is off.
- Gallery: upload and delete-all are gated on a degraded gallery; gallery add rejects unreadable and absolute folder paths; page-jump dialog clamps out-of-range entries; the toolbar wraps on narrow viewports; the top-nav stays reachable on narrow viewports; sync-missing now labels images "missing" instead of "removed".
- Sidebar: source-filter tree shows per-source counts; the `[·]` shortcut for `folderonly:PATH` is now visible at the same size as the folder name instead of a hover-only middle dot.
- Watcher now watches every configured gallery, not just the active one.
- Auto-tagger: empty subfolders are hidden; unavailable rows are marked n/a; the detail-page Auto-tag button is hidden in the noop build with the real reason surfaced.
- API: `/api/v1/docs` shows a banner when the API is disabled and gets a Back link at the top; category-qualified DELETE falls through to a literal match when the qualified lookup misses.
- Web: missing `_hover.webp` thumbnails return 204; random sort is visible in the gallery sort dropdown; Save-search and Delete-all hide when there's no query or empty result set; job-flash auto-dismiss shortens once a client has seen it.

## [v1.2.0] - 2026-04-18

### Added
- Move images into another folder from the UI: **Move image** in the detail page's action row, **Move selected** in the gallery's batch bar. Folder input autocompletes against existing folders; missing folders are created, filename collisions are auto-suffixed, and empty source folders are cleaned up after the move.

### Changed
- Sidebar folder tree: each node's count now includes images in every descendant folder, so a parent with only subfolder content shows a non-zero figure. `folder:PATH` in the search bar is recursive to match. `folder:` with no value is now a recursive root (matches every non-missing image); use the new `folderonly:` for the old root-only match.
- Each folder row in the sidebar (including `/`) now has a `·` shortcut that runs `folderonly:PATH` to show only the images directly in that folder, without the rolled-up subfolder content.

### Fixed
- Sidebar folder tree: deeply nested folders no longer slide off the right edge of the sidebar. Indent is now a fixed 12 px per level instead of the quadratic `depth × 12` that accumulated through nested `<li>` padding boxes.

## [v1.1.0] - 2026-04-18

### Added
- Multi-gallery support. Galleries are named directories with their own SQLite DB and thumbnails under `<paths.data_path>/<name>/`. Create, rename, and delete galleries from Settings → Galleries; switch at runtime with the topbar button. The REST API targets a specific gallery via `?gallery=<name>` or the `X-Monbooru-Gallery` header; unknown names return 400.
- Daily maintenance schedule: pick a time of day to run sync, auto-tag, recompute counts, merge general tags, and vacuum. All actions enabled by default; toggle per-action in Settings.
- New search filters `tagged:true/false` and `autotagged:true/false` to scope results by tagging state.
- Sync, delete, and re-extract jobs are now cancellable from the status bar, matching the existing auto-tag cancel behavior.
- Sync reconcile reports live progress and the gallery grid refreshes mid-run; delete and re-extract jobs also refresh the grid in-flight, so ingested or removed images appear as jobs run.
- Auto-tag groups on the image detail page are ordered by confidence and show a percentage next to each tag.

### Changed
- Gallery configuration no longer stores `db_path` or `thumbnails_path`. Each gallery lives under `<paths.data_path>/<name>/monbooru.db` + `/thumbnails/`, created on demand. `active_gallery` is renamed to `default_gallery` and only controls the startup pick; the topbar switcher changes the runtime active gallery without persisting. Legacy `[paths]` migration is removed - existing `monbooru.toml` files must be rewritten as `[[galleries]]` entries on a fresh config.
- Settings → Auto-Tagger section now sits above Authentication.
- "Delete all" is hidden while a batch-delete selection is in progress, to avoid two conflicting destructive buttons.
- Sync on large libraries: duplicate hashing and per-file `chown` are skipped; alias lookups and missing-row updates are batched, so idle syncs on 50k+ libraries finish in seconds rather than minutes.
- Gallery page caches the unfiltered visible count and the per-gallery folder tree, cutting redundant SQL scans on every render.

### Fixed
- Search: chained `OR` terms parse correctly (the tail of a 3+ term chain was being dropped). Non-numeric `width:`/`height:` filters are rejected with a clear error. `-missing:false` returns missing images. Autocomplete drops tags already in the query; the suggest swap target is pinned with `HX-Retarget` so results render in the correct place.
- Errors from form parsing, cursor iteration, folder deletion, config save, tagger result storage, and tag prune now surface to the user instead of being silently dropped.
- Tags: add-tag check is atomic with the insert (no read/write-pool race). `MergeTags` counts non-missing rows only. Typed filter input survives a category change on the tags page.
- Tagger: label filenames are sanitised before they become tag rows. Discovered taggers are preserved across a settings save.
- Jobs: scheduled maintenance holds a schedule reservation while running so a manual action cannot start mid-flight. Rebuild-thumbs honors cancellation. Sync respects cancellation during mark-missing and tag recalc. Job-status auto-clear re-arms on every surface event. Watcher ingests surface while a long-running job is in flight. Scheduler cancellation reports a clean summary instead of "context canceled".
- Gallery: out-of-range page numbers clamp to the last valid page. Switching gallery from the image detail page redirects to home. Per-gallery thumbnail URLs render correctly after a switch; switch errors surface in a flash. Sidebar and tag-list search links carry the active category prefix.
- Web: WAL is truncated after vacuum and the total database footprint is reported in the flash. Upload and categories pages receive the gallery switcher data. Settings → Galleries shows the API suffix instead of the full URL. `DeleteImage` no longer runs an unscoped tag prune.
- Sync progress bar no longer double-counts the reconcile phase.

### Removed
- `MONBOORU_PATHS_GALLERY`, `MONBOORU_PATHS_DB`, and `MONBOORU_PATHS_THUMBNAILS` env overrides. Replaced by `MONBOORU_PATHS_DATA_PATH`.

## [v1.0.0] - 2026-04-16

Initial release.
