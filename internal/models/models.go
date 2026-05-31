package models

import (
	"net/url"
	"time"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

const (
	FileTypeJPEG = "jpeg"
	FileTypePNG  = "png"
	FileTypeWEBP = "webp"
	FileTypeGIF  = "gif"
	FileTypeMP4  = "mp4"
	FileTypeWEBM = "webm"
	// FileTypeCBZ covers `.cbz` and `.zip` archives ingested as a single
	// manga row. Page bytes are extracted lazily into a per-image cache;
	// the cover thumbnail is page 1.
	FileTypeCBZ = "cbz"

	SourceTypeA1111   = "a1111"
	SourceTypeComfyUI = "comfyui"
	SourceTypeNone    = "none"
	// SourceTypeBoth is used when an image has both A1111 and ComfyUI metadata.
	SourceTypeBoth = "a1111,comfyui"

	// OriginIngest is recorded for files the watcher or Sync picks up from
	// disk; also the Ingest() default when no explicit origin is supplied.
	OriginIngest = "ingest"
	// OriginUpload is recorded for web-UI uploads and is the default for
	// multipart API uploads.
	OriginUpload = "upload"
)

type Image struct {
	ID            int64
	SHA256        string
	CanonicalPath string
	FolderPath    string // relative dir from gallery_path root; "" = root
	FileType      string // "jpeg" | "png" | "webp" | "gif" | "mp4" | "webm" | "cbz"
	Width         *int
	Height        *int
	FileSize      int64
	IsMissing     bool
	IsFavorited   bool
	IsInbox       bool // 1 = needs triage; 0 = archived/curated
	AutoTaggedAt  *time.Time
	SourceType    string   // "a1111" | "comfyui" | "none" | "a1111,comfyui"
	Origin        string   // "ingest" | "upload" | caller-supplied string (app name, URL...)
	Source        string   // free-form provenance label (site name, scraper, ...); operator-edited
	URL           string   // canonical web URL the image came from; http(s) only
	PageCount     *int     // page entry count for cbz manga rows; NULL otherwise
	DurationSec   *float64 // video duration in seconds; NULL for non-video rows and for videos that pre-date the column or whose probe failed
	Series        string   // operator-edited free-form series label (max 200 chars); '' when unset
	SeriesOrder   *int     // operator-edited position within Series; NULL = unspecified
	Phash         *int64   // 64-bit perceptual hash; NULL until backfilled or for rows without a decodable thumbnail
	IngestedAt    time.Time
}

// MangaMetadata mirrors sd_metadata / comfyui_metadata for the manga
// feature: parsed read-only ComicInfo.xml descriptors surfaced on the
// detail page. The authoritative page count lives on Image.PageCount;
// XMLPageCount is whatever the XML declared and is shown for
// information only.
type MangaMetadata struct {
	ImageID         int64
	Title           string
	Series          string
	Number          string
	Volume          string
	Count           *int
	Summary         string
	Notes           string
	Year            *int
	Month           *int
	Day             *int
	Writer          string
	Penciller       string
	Inker           string
	Colorist        string
	Letterer        string
	CoverArtist     string
	Editor          string
	Publisher       string
	Imprint         string
	Genre           string
	Web             string
	LanguageISO     string
	Format          string
	Manga           string // "Yes" | "YesAndRightToLeft" | "No" | "Unknown"
	AgeRating       string
	CommunityRating *float64
	XMLPageCount    *int
	RawXML          string // full XML body verbatim, capped at 64 KiB at parse time
}

type ImagePath struct {
	ID          int64
	ImageID     int64
	Path        string
	IsCanonical bool
}

type Tag struct {
	ID                    int64
	Name                  string
	CategoryID            int64
	CategoryName          string
	CategoryColor         string
	UsageCount            int
	IsAlias               bool
	IsAutoOnly            bool // true if all usages of this tag are auto-tagged (no manual usage)
	IsAPIOnly             bool // true if every manual usage carries an API source label (no anonymous UI add)
	CanonicalTagID        *int64
	CanonicalName         string // populated on alias rows when ListTags joins the canonical
	CanonicalCategoryName string
	CanonicalCategoryColor string
	CreatedAt             time.Time
}

type TagCategory struct {
	ID        int64
	Name      string
	Color     string
	IsBuiltin bool
}

type ImageTag struct {
	ImageID    int64
	TagID      int64
	TagName    string
	Category   string
	Color      string
	UsageCount int
	IsAuto     bool
	IsImplied  bool // row was fanned out from a parent tag's implication graph
	Confidence *float64
	TaggerName string // source auto-tagger when IsAuto; empty for manual tags
	CreatedAt  time.Time
}

// Implication is one edge of the tag implication graph: adding ParentID
// to an image fans out an implied row for ImpliedID.
type Implication struct {
	ParentID            int64
	ImpliedID           int64
	ParentName          string
	ParentCategoryName  string
	ParentCategoryColor string
	ImpliedName         string
	ImpliedCategoryName string
	ImpliedCategoryColor string
	CreatedAt           time.Time
}

// SDParam is a single parsed key-value pair from A1111 generation parameters.
type SDParam struct {
	Key string
	Val string
}

type SDMetadata struct {
	ImageID        int64
	Prompt         string
	NegativePrompt string
	Model          string
	Seed           *int64
	Sampler        string
	Steps          *int
	CFGScale       *float64
	RawParams      string    // full A1111 parameter line for display
	ParsedParams   []SDParam // all key-value pairs parsed from RawParams
	GenerationHash string    // short hex digest over prompt/model/sampler/steps/cfg (seed excluded)
}

type ComfyUIMetadata struct {
	ImageID         int64
	Prompt          string
	ModelCheckpoint string
	Seed            *int64
	Sampler         string
	Steps           *int
	CFGScale        *float64
	RawWorkflow     string
	GenerationHash  string // short hex digest over prompt/model/sampler/steps/cfg (seed excluded)
}

// ComfyNode represents one node from a ComfyUI workflow for structured display.
type ComfyNode struct {
	Key       string
	Title     string
	ClassType string
	Params    []ComfyNodeParam
}

// ComfyNodeParam is a single input parameter on a ComfyUI node.
type ComfyNodeParam struct {
	Name  string
	Value string
	IsRef bool // true if the value is a reference to another node
}

type SavedSearch struct {
	ID        int64
	Name      string
	Query     string
	Sort      string
	Order     string
	Seed      string
	CreatedAt time.Time
}

// HRef builds the `/?...` link a sidebar entry resolves to. Mirrors the
// gallery handler's URL contract so reopening the entry lands the user
// on the same view they saved (q + sort + order + seed).
func (s SavedSearch) HRef() string {
	out := "/?q=" + urlQueryEscape(s.Query)
	if s.Sort != "" {
		out += "&sort=" + urlQueryEscape(s.Sort)
	}
	if s.Order != "" {
		out += "&order=" + urlQueryEscape(s.Order)
	}
	if s.Seed != "" {
		out += "&seed=" + urlQueryEscape(s.Seed)
	}
	return out
}

// Background-job type identifiers. Use these constants instead of bare
// strings at every jobs.Start / StartScheduled call site so a typo
// surfaces at compile time.
const (
	JobTypeSync          = "sync"
	JobTypeAutotag       = "autotag"
	JobTypeReExtract     = "re-extract"
	JobTypeDelete        = "delete"
	JobTypeRebuildThumbs = "rebuild-thumbs"
	JobTypeMove          = "move"
	JobTypeTag           = "tag"
	JobTypeWatcher       = "watcher"
	JobTypePruneThumbs   = "prune-thumbs"
	JobTypeVacuum        = "vacuum"
	JobTypeFreeMemory    = "free-memory"
	JobTypePhash         = "phash"
	JobTypeRelations     = "relations"
)

type JobState struct {
	Running    bool
	JobType    string // one of the JobType* constants above
	Total      int
	Processed  int
	Message    string
	StartedAt  time.Time
	FinishedAt *time.Time
	Summary    string
	Error      string
	// WatcherNotices is a monotonic counter bumped on every watcher
	// ingest/remove that fires while a job is running. The client uses it
	// as a refresh signal without overwriting the running progress line.
	WatcherNotices int
}

type SearchResult struct {
	Page    int
	Limit   int
	Total   int
	Results []Image
}

