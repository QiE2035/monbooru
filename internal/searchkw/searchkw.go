// Package searchkw declares the search query language's filter-keyword
// vocabulary as a leaf data package. The parser and executor in
// internal/search read it to dispatch `key:value` filters; the gallery
// search bar's `system:` cheat-sheet iterates Keywords for its dropdown
// rows; the tag service in internal/tags borrows the same list as the
// reserved category-name set. Putting the source of truth here breaks
// the cycle that would otherwise form via internal/gallery's tag-service
// usage and internal/search's gallery-using test fixtures.
package searchkw

// Keywords lists every search-filter keyword in the order the
// `system:` cheat-sheet dropdown surfaces them. Adding a future filter
// is one edit here.
var Keywords = []string{
	"fav",
	"inbox",
	"ai",
	"source",
	"cat",
	"width",
	"height",
	"date",
	"missing",
	"tagged",
	"autotagged",
	"folder",
	"folderonly",
	"generated",
	"rating",
	"type",
	"collection",
	"pages",
	"name",
	"size",
	"mime",
	"ratio",
	"tagcount",
	"duration",
	"hash",
	"prompt",
	"model",
	"sampler",
	"seed",
	"via",
	"phash",
	"relation",
	"id",
}

// keywordSet is the membership-test view of Keywords. Built once at
// init so IsKeyword is a single map lookup.
var keywordSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Keywords))
	for _, k := range Keywords {
		m[k] = struct{}{}
	}
	return m
}()

// IsKeyword reports whether s is one of the recognised search-filter
// keywords. Returns false for tag-name colons like "nier:automata".
func IsKeyword(s string) bool {
	_, ok := keywordSet[s]
	return ok
}

// Expansions lists the second-level autocomplete rows for the
// `system:<key>:` cheat-sheet drill-in: comparison operators for ordinal
// filters and closed-vocabulary values otherwise. `cat:` is data-driven
// from `tag_categories` and intentionally absent here; `folder:`,
// `folderonly:`, and `generated:` accept open input and have no static
// expansion.
var Expansions = map[string][]string{
	"fav":        {"true", "false"},
	"inbox":      {"true", "false"},
	"ai":         {"a1111", "comfyui", "none", "any", "sd"},
	"width":      {">=", "<=", ">", "<", "="},
	"height":     {">=", "<=", ">", "<", "="},
	"date":       {">", "<", ">=", "<=", "=", ".."},
	"missing":    {"true", "false"},
	"tagged":     {"true", "false"},
	"autotagged": {"true", "false"},
	"rating":     {"general", "sensitive", "questionable", "explicit"},
	"type":       {"image", "archive", "animated"},
	"pages":      {">=", "<=", ">", "<", "="},
	"size":       {">=", "<=", ">", "<", "="},
	"ratio":      {">=", "<=", ">", "<", "="},
	"tagcount":   {">=", "<=", ">", "<", "="},
	"duration":   {">=", "<=", ">", "<", "="},
	"mime":       {"jpeg", "png", "webp", "gif", "mp4", "webm", "cbz"},
	"via":        {"ingest", "upload"},
	"relation":   {"duplicate", "original", "alternate", "version", "derivative", "source", "collection", "any", "none"},
}

// Descriptions maps each filter keyword to a short English label the
// cheat-sheet dropdown shows just left of the "system" column. Tag
// categories surface in the level-1 list with a generic "category"
// label applied at render time.
var Descriptions = map[string]string{
	"fav":        "favorite images",
	"inbox":      "in inbox",
	"ai":         "AI generation tool",
	"source":     "external source label",
	"cat":        "by tag category",
	"width":      "image width",
	"height":     "image height",
	"date":       "ingestion date",
	"missing":    "files gone from disk",
	"tagged":     "has any tag",
	"autotagged": "has auto-tag",
	"folder":     "folder (recursive)",
	"folderonly": "folder (exact)",
	"generated":  "generation recipe",
	"rating":     "safety rating",
	"type":       "image or archive",
	"collection": "collection label",
	"pages":      "page count",
	"name":       "file name",
	"size":       "file size",
	"mime":       "file type",
	"ratio":      "aspect ratio (width / height)",
	"tagcount":   "number of tags",
	"duration":   "video duration in seconds",
	"hash":       "sha256 digest",
	"prompt":     "SD / ComfyUI prompt",
	"model":      "SD / ComfyUI model",
	"sampler":    "SD / ComfyUI sampler",
	"seed":       "SD / ComfyUI seed",
	"via":        "added via",
	"phash":      "perceptual hash (16-hex; ~d for Hamming distance)",
	"relation":   "declared relation",
	"id":         "image id",
}

// ExpansionDescriptions maps level-2 rows to a short English label.
// Boolean expansions (true / false) and rating values are intentionally
// absent - they're self-explanatory in their bare form. Operators and
// `source:` values benefit from disambiguation.
var ExpansionDescriptions = map[string]map[string]string{
	"date": {
		">":  "after",
		"<":  "before",
		">=": "on or after",
		"<=": "on or before",
		"=":  "exactly",
		"..": "range",
	},
	"width": {
		">=": "at least",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
	},
	"height": {
		">=": "at least",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
	},
	"ai": {
		"a1111":   "A1111 / Forge",
		"comfyui": "ComfyUI",
		"none":    "no metadata",
		"any":     "any AI tool",
		"sd":      "alias of a1111",
	},
	"pages": {
		">=": "at least",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
	},
	"type": {
		"image":    "regular images and videos",
		"archive":  "cbz / zip archives",
		"animated": "gif / mp4 / webm",
	},
	"size": {
		">=": "at least (bytes; suffix KB/MB/GB)",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
	},
	"ratio": {
		">=": "wider than (e.g. 1.5 = 3:2)",
		"<=": "taller than",
		">":  "wider than",
		"<":  "taller than",
		"=":  "exact ratio",
	},
	"tagcount": {
		">=": "at least",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
	},
	"duration": {
		">=": "at least N seconds",
		"<=": "at most N seconds",
		">":  "longer than",
		"<":  "shorter than",
		"=":  "exactly N seconds",
	},
	"via": {
		"ingest": "watcher or sync",
		"upload": "web upload form",
	},
}
