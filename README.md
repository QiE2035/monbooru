# Monbooru

Your own self-hosted, lightweight and fast booru.  
Designed for organizing your local media collection, including AI-generated images (Stable Diffusion, ComfyUI, A1111/Forge). Completely offline by design. Supports ONNX models for local auto-tagging of your collection (WD14, JoyTag...).  

<table>
  <tr>
    <td><img src="/.github/assets/gallery.webp" width="400"/></td>
    <td><img src="/.github/assets/image.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/sd.webp" width="400"/></td>
    <td><img src="/.github/assets/tags.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/inbox.webp" width="400"/></td>
    <td><img src="/.github/assets/compare.webp" width="400"/></td>
  </tr>
</table>

---

## Features

- **Tag-based gallery** with folder tree, favorites, saved searches, collections, related-image suggestions, and rating tags (general / sensitive / questionable / explicit) with a SFW ceiling
- **Stable Diffusion metadata** from A1111/Forge and ComfyUI: prompts, models, seeds, full workflows
- **Local ONNX auto-tagging** (WD14 SwinV2, JoyTag, Camie v2, or any compatible model) on CPU or GPU
- Images, video, animated GIFs, plus CBZ/ZIP archives browsed as a single object with a built-in manga reader; animated hover previews on the grid
- **Search** with wildcards, OR, exclusions, plus filters on folder, date, size, dimensions, category, generation recipe, and more
- **Tag management**: merge duplicates, create aliases, implications, custom categories
- **Image relations**: declared duplicates, alternates, version chains, derivatives; plus near-duplicate detection
- **Batch operations** on a selection or a whole search: delete, move, add/remove tags, auto-tag
- **File watcher** that catches new, moved, and deleted files within seconds; multi-file browser upload; SHA-256 dedup so the same file under two paths is one image
- **Inbox workflow** : new images land in the inbox for review
- **Multiple galleries** in one instance, each with its own filesystem and database; per-gallery export/import; import data from supported booru applications
- **REST API** for third-party integrations
- **Optional password login**

---

## Quick start (Docker)

Edit the volume paths in [`docker/docker-compose.yml`](docker/docker-compose.yml), then `docker compose up -d`. The app is available at `http://localhost:8080`.

See [docs/README.md](docs/README.md) for installing, configuration, auto-tagger setup, environment variables, REST API, building from source, and other operational notes. Check the in-app help to see how to use the application (search syntax, tag input, keyboard shortcuts, ...).

---

## Warning

> **Intended for local network use.** This project is not designed for direct exposure to the public internet.

---

## Acknowledgements

Thanks to [@gary-host-laptop](https://github.com/gary-host-laptop) for ongoing feature requests and feedback.
