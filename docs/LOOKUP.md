# Lookups

Monbooru can reverse-search your own images against external sources to
backfill their tags and sources. Every lookup runs through a paired
[monloader](https://github.com/leqwin/monloader) instance, so set one up
first (see [Installing](INSTALL.md) and [REST API](API.md)) - the actions
below stay hidden until the pairing is live.

## What a lookup searches

Two backends, used together or on their own depending on where the lookup
starts:

- **Online boorus and similarity services** - matches the file by its
  md5, and by image similarity through IQDB and SauceNAO. A booru hit is
  recorded as a new source on the image and its tags are merged in.
- **Hydrus Public Tag Repository (PTR)** - matches the file by its
  sha256. Only available when monloader has the PTR enabled. Pulls the
  matched post's tags, and on the Tags page a tag's aliases and
  implications.

Boorus index the original file's hash, so a resized or re-encoded copy
usually misses on hash and only turns up similarity candidates. A miss
reports what was searched together with the file's hashes; a similarity
candidate carries a `~NN%` match score.

## How to run it

- **Detail page, Lookup** - offered in the tag editor whenever a
  monloader is paired. Searches boorus by md5 and, when enabled,
  the PTR by sha256. With the PTR enabled the button opens a small dialog
  to pick **both**, **PTR only**, or **online boorus** (with no PTR it
  runs the online boorus straight away). A found post's tags are merged
  and a booru match is recorded as a source.
- **Detail page, source refresh** - each source row with a URL has a
  **refresh** action that re-pulls that post's tags, commentary and
  notes. A `ptr` source needs no URL and refreshes by sha256; a
  similarity-matched source (`~NN%`) skips the usual same-file check.
- **Batch Lookup** - the gallery **Actions** chooser and the selection
  batch bar run a lookup across the whole scope: refresh tags from each
  image's primary source, or run the hash and similarity lookup per
  image. With the PTR enabled the hash lookup can be narrowed to the
  **PTR only** or **online boorus only**, so a large scope can stay on
  the free local index or spare the online services.
- **Tags page, PTR lookup** - pulls a tag's known aliases and
  implications from the PTR, per row or for the whole current search. 

A booru post that names a parent post links the two as a derivative
relation once both are in the gallery - see [Relations](RELATIONS.md).
