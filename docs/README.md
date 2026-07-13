# Monbooru docs

See these pages for guidance on how to set up monbooru. Help on how to use the application lives in the app at `/help`.

- [Installing](INSTALL.md) - Docker quickstart, configuration file, volumes,
  environment variables, log levels, custom CSS.
- [Building from source](BUILDING.md) - Go build commands, CLI flags, ONNX
  Runtime requirements.
- [Galleries](GALLERIES.md) - adding, switching, exporting, importing,
  merging galleries. Format details.
- [Auto-tagger](AUTO-TAGGER.md) - installing taggers, thresholds, per-gallery
  scoping, dispatcher overrides, GPU (CUDA), idle release.
- [Relations](RELATIONS.md) - declared duplicates, alternates, version chains,
  derivatives; perceptual-hash; the find-pairs job and the swipe
  session UI; the two duplicates walkers.
- [Lookups](LOOKUP.md) - reverse-searching your images against
  boorus, similarity services, and the Hydrus PTR to backfill tags and
  sources.
- [Maintenance, Schedule, Stats, and Troubleshooting](MAINTENANCE.md) - the
  manual maintenance tools, the nightly schedule, the diagnostic Stats
  block, and common failure modes.
- [REST API](API.md) - enabling the API, authentication, endpoints.
- [Migrating from third party applications](MIGRATING.md) - importing foreign
  exports.
