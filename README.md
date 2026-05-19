# Monbooru

Your own self-hosted, lightweight and fast booru.  
Designed for organizing your local media collection, including AI-generated images (Stable Diffusion, ComfyUI, A1111/Forge). Works fully offline. Supports ONNX models for local auto-tagging of your collection (WD14, JoyTag...).  

<table>
  <tr>
    <td><img src="/.github/assets/gallery.webp" width="400"/></td>
    <td><img src="/.github/assets/image.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/sd.webp" width="400"/></td>
    <td><img src="/.github/assets/tags.webp" width="400"/></td>
  </tr>
</table>

---

## Features

- Tag-based gallery with folder tree, favorites, saved searches, related-image suggestions
- Videos and animated GIFs alongside images, with animated hover previews on the grid
- Manga / comic / archive support: browse CBZ / ZIP archives as a single object with a built-in reader
- Stable Diffusion metadata extraction from A1111/Forge and ComfyUI (prompts, models, seeds, full workflow)
- Tag management: merge duplicates, create aliases, implications and custom categories
- Batch operations on a selection or on a whole search: delete, move, add/remove tags, auto-tag
- Optional auto-tagging with local ONNX models (WD14 SwinV2, JoyTag, Camie v2, or any compatible model), CPU or GPU (CUDA)
- Watcher that picks up new, moved, and deleted files within a few seconds; multi-file browser upload; SHA-256 dedup so the same file under two paths is recognised as one image
- Inbox system: new ingests land in the inbox; flip to archived once curated.
- Search with wildcards, OR, exclusions, plus filters on folder, date, size, dimensions, category, generation recipe...
- Image relations: declared duplicates, alternates, version chains, and derivative edges. Near-duplicates are computed using perceptual hashes.
- Collections with an optional within-collection order
- Built-in rating tags (general / sensitive / questionable / explicit) with a SFW ceiling toggle in the footer that hides anything above the chosen level
- Multiple galleries, each with its own filesystem and database; switch between them at runtime
- Per-gallery export and import for backup or migration. The importer also reads Hydrus network and Blombooru exports - see [docs/MIGRATING.md](docs/MIGRATING.md)
- REST API for third-party integrations (e.g. adding images to the gallery from an external app)
- Optional password login
- Fully offline, no inbound or outbound internet connection

---

## Quick start (Docker)

Edit the volume paths in [`docker/docker-compose.yml`](docker/docker-compose.yml), then `docker compose up -d`. The app is available at `http://localhost:8080`.

See [docs/README.md](docs/README.md) for installing, configuration, auto-tagger setup, environment variables, REST API, building from source, and other operational notes. Check the in-app help to see how to use the application (search syntax, tag input, keyboard shortcuts, ...).

---

## Warning

> **Intended for local network use.** This project is not designed for direct exposure to the public internet.
