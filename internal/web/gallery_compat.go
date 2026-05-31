package web

import (
	"archive/zip"
	"maps"
	"slices"

	"github.com/leqwin/monbooru/internal/web/compatibility"
)

// Importing the compatibility package here runs its providers' init()
// functions, registering the per-application translators.
func detectCompatFormat(files []*zip.File) string {
	return compatibility.Detect(files)
}

// replaceFromCompatArchive routes a foreign-format zip through the native
// light-replacer path. format is propagated to applyLightReplace so the
// detail page credits the originating app instead of the generic "import".
func replaceFromCompatArchive(files []*zip.File, format, dbPath, thumbsPath, galleryPath string, maxFileSizeMB int) error {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return err
	}
	return applyLightReplace(
		toLightManifest(result.Manifest),
		translatedFilesFromCompat(result.Files),
		dbPath, thumbsPath, galleryPath, format, maxFileSizeMB,
	)
}

// mergeFromCompatArchive routes a foreign-format zip through the zip-merge
// path: tags onto existing SHAs, ingest-and-tag for new SHAs.
func mergeFromCompatArchive(cx *galleryCtx, files []*zip.File, format string, maxFileSizeMB int) error {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return err
	}
	records := make([]mergeRecord, 0, len(result.Manifest.Images))
	for _, m := range result.Manifest.Images {
		rec := mergeRecord{
			SHA256:     m.SHA256,
			Tags:       m.Tags,
			SourcePath: m.Path,
		}
		if zf, ok := result.Files[m.Path]; ok {
			rec.zipEntry = zf
		}
		records = append(records, rec)
	}
	applyMergeRecords(cx, records, format, maxFileSizeMB)
	return nil
}

func toLightManifest(m compatibility.Manifest) lightManifest {
	out := lightManifest{
		Version: lightManifestVersion,
		Images:  make([]lightManifestImage, 0, len(m.Images)),
	}
	for _, img := range m.Images {
		out.Images = append(out.Images, lightManifestImage{
			SHA256: img.SHA256,
			Path:   img.Path,
			Tags:   img.Tags,
		})
	}
	return out
}

func translatedFilesFromCompat(in map[string]*zip.File) []translatedFile {
	rels := slices.Sorted(maps.Keys(in))
	out := make([]translatedFile, 0, len(rels))
	for _, r := range rels {
		out = append(out, translatedFile{rel: r, file: in[r]})
	}
	return out
}

