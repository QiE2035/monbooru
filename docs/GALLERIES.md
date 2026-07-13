# Galleries

A gallery is a named folder plus its own SQLite DB and thumbnails. Each
gallery is independent - its own images, tags, saved searches, and so
on.

Manage them at **Settings → Galleries**:

- **Add** - give it a name and a path on disk. The Add form also
  accepts an import file (.db / .sqlite / .json / .zip) so you can
  create a populated gallery in one shot. Adding a gallery switches the
  active gallery to it (runtime only; the default is restored on
  restart).
- **Switch** - runtime only; doesn't persist.
- **Set default** - the gallery that loads on startup.
- **Rename** - give it a new name.
- **Delete** - type the gallery name to confirm. The database and
  thumbnails are erased; tick **Also delete the gallery folder (source
  images)** to remove the source files from disk too. The active,
  default, and last remaining gallery can't be deleted - switch or
  demote one first.

The footer shows the active gallery; click it to switch when more than
one is configured.

## Export

**Export** offers three formats:

- `format=db` - SQLite snapshot (`VACUUM INTO`).
- `format=json` - single JSON document with every table, relations and
  collections included.
- `format=light` - portable bundle: a bare `tags.json` listing
  `{sha256, path, tags}` per image, with monbooru-specific data
  (SD/ComfyUI metadata, saved searches, tag attribution, relations)
  stripped.
  Useful as a tags-only backup or as input for other software. Only
  images present on disk are listed; rows whose file is missing are
  skipped.

`with_images=true` bundles any of the three formats plus every file under the
gallery into a `.zip`. Thumbnails are deliberately excluded. 
For `format=light` without `with_images`, the response is the bare `tags.json`.

## Import

**Import** accepts a `.db`, `.sqlite`, `.json`, or `.zip`, plus two
foreign-format zips:

- A Blombooru `full_backup.zip`.
- A zipped Hydrus export folder (media files plus `.txt` sidecars).

A `.json` upload may be either a full monbooru export or a bare
`tags.json` light manifest; the importer sniffs the document and routes
accordingly. For the foreign formats and the light manifest, only image
bytes and tags are preserved. See [MIGRATING.md](MIGRATING.md) for
details.

The dialog has two modes:

- **Merge** (the dialog's preselected radio) - add new images and tags
  from the upload to the target gallery. Existing rows are kept; tags
  from the upload are layered onto matching SHA-256 rows. A `.db` or
  `.json` upload carries no image bytes, so it applies tags only; new
  images arrive only from an upload that bundles files (a `.zip`). A
  successful merge also switches the active gallery to the target. 
- **Replace** - wipe the target gallery's DB and thumbnails, then load
  the upload into the empty gallery. When the upload bundles image files
  (a `.zip`, or a Hydrus / Blombooru archive), the gallery's source
  folder is wiped and repopulated too; a `.db` / `.json` upload leaves
  the source files in place. Type-to-confirm the gallery's name. Refused
  on the active gallery and the default gallery (switch / demote first).

After Replace finishes, monbooru switches the active gallery to the
freshly imported one and kicks off a thumbnail rebuild in the background.
Import (either mode) is refused while any background job is running (the
job lane is shared across galleries).

A manifest or foreign-format entry whose file isn't present on disk is
still imported as a tagged row flagged missing; surface those with
`missing:true`. Individual files inside an archive are capped at the
configured `max_file_size_mb`, and the whole upload at 16 GiB.

## Transfer

To move a few images into another gallery without an export/import
cycle, use **Transfer**. It appears on the detail page (**Transfer to
another gallery**) and in the gallery batch bar (on a selection or a
whole search) whenever more than one gallery is configured.

The image is copied into the target gallery and re-ingested there, so it
gets a fresh thumbnail and its own id. Its tags, rating, sources, artist
commentary, annotations, note, original source and favorite come along. If the same file
is already in the target (matched by SHA-256), the transfer merges its
tags and provenance into that row instead of adding a duplicate, and
won't overwrite a note, original source, or favorite the row already has.

**Relations and collections are not carried over.** They point at other
images that may not exist in the target gallery, so the transferred image
lands in the target's inbox for you to re-file. Declared duplicates,
version chains and collection membership stay in the source gallery.

Tick **Remove from this gallery after transfer** to move instead of
copy: the source image is deleted once the copy succeeds. The transferred
image lands in the target's inbox whether you copy or move, since its
relations and collections have to be re-filed there.
