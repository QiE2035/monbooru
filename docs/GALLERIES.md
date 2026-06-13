# Galleries

A gallery is a named folder plus its own SQLite DB and thumbnails. Each
gallery is independent - its own images, tags, saved searches, and so
on.

Manage them at **Settings → Galleries**:

- **Add** - give it a name and a path on disk. The Add form also
  accepts an import file (.db / .json / .zip) so you can create a
  populated gallery in one shot. Adding a gallery switches the active
  gallery to it (runtime only; the default is restored on restart).
- **Switch** - runtime only; doesn't persist.
- **Set default** - the gallery that loads on startup.
- **Rename** / **Delete** - type the gallery name to confirm.

The footer shows the active gallery; click it to switch when more than
one is configured.

REST API requests can target any gallery via `?gallery=<name>` or the
`X-Monbooru-Gallery` header.

## Export

**Export** offers three formats:

- `format=db` - SQLite snapshot (`VACUUM INTO`).
- `format=json` - single JSON document with every table.
- `format=light` - portable bundle: a bare `tags.json` listing
  `{sha256, path, tags}` per image, with monbooru-specific data
  (SD/ComfyUI metadata, saved searches, tag attribution) stripped.
  Useful as a tags-only backup or as input for other software.

`with_images=true` bundles either format plus every file under the
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
  from the upload are layered onto matching SHA-256 rows.
- **Replace** - wipe the target gallery's DB and thumbnails, then load
  the upload into the empty gallery. Type-to-confirm the gallery's
  name. Refused on the active gallery and the default gallery (switch
  / demote first).

After Replace finishes, monbooru kicks off a thumbnail rebuild in the
background and switches the active gallery to the freshly imported one.
