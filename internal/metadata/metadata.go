package metadata

import (
	"os"
	"sort"

	"github.com/monbooru/monbooru/internal/models"
)

// Extract reads SD and/or ComfyUI metadata from a file. Either return
// can be nil; parsing is best-effort and failures surface as nil rather
// than errors.
func Extract(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata, error) {
	switch fileType {
	case "png":
		sd, comfy, err := extractFromPNG(path)
		return sd, comfy, err
	case "jpeg":
		sd, err := extractSDFromJPEG(path)
		return sd, nil, err
	case "webp":
		return extractSDFromWebP(path), nil, nil
	default:
		return nil, nil, nil
	}
}

// ExtractGeneric returns key-value pairs the SD and ComfyUI parsers
// don't consume (extra PNG text chunks, EXIF tags). Best-effort; file
// errors return an empty slice.
func ExtractGeneric(path, fileType string) []models.SDParam {
	switch fileType {
	case "png":
		return genericFromPNG(path)
	case "jpeg":
		return genericFromEXIF(path)
	case "webp":
		return genericFromWebP(path)
	default:
		return nil
	}
}

func genericFromPNG(path string) []models.SDParam {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	chunks, err := readPNGTextChunks(f)
	if err != nil {
		return nil
	}
	skip := map[string]bool{"parameters": true, "prompt": true, "workflow": true}
	keys := make([]string, 0, len(chunks))
	for k := range chunks {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]models.SDParam, 0, len(keys))
	for _, k := range keys {
		out = append(out, models.SDParam{Key: k, Val: chunks[k]})
	}
	return out
}

func genericFromEXIF(path string) []models.SDParam {
	x := decodeJPEGEXIF(path)
	if x == nil {
		return nil
	}
	return collectEXIFTags(x)
}

// collectEXIFTags walks every EXIF tag across all IFDs and returns them
// as key/value pairs. UserComment is skipped because it's the SD source.
func collectEXIFTags(x *exifData) []models.SDParam {
	out := make([]models.SDParam, 0, len(x.tags))
	x.walk(func(name string, tag *exifTag) {
		if name == userCommentField {
			return
		}
		out = append(out, models.SDParam{Key: name, Val: tag.String()})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
