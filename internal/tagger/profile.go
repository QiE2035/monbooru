package tagger

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/logx"
)

// profileSchemaVersion is the only schema version the loader honours.
// Documents on a different version are skipped so a future schema change
// can roll out without a stampede of unrunnable taggers.
const profileSchemaVersion = 1

// Profile captures every axis where ONNX taggers actually differ:
// preprocessing (input size, layout, channel order, normalisation, padding),
// model output shape (which output slot to read, sigmoid in-model vs raw
// logits), label-file format, and how labels declare their category.
//
// A profile is resolved once per (tagger, file set) and pinned by SHA in
// the session cache so a sidecar edit forces a session rebuild. Empty
// strings mean "fall through to the previous resolution layer"; an empty
// final profile is invalid and surfaces at session-build time.
type Profile struct {
	Name string `json:"-"`

	// InputSize is the model's expected square input edge in pixels.
	// Zero means "ask ONNX at session-build time" (cache.ensure reads
	// inputs[0].Dimensions and picks the spatial axis).
	InputSize int `json:"input_size,omitempty"`

	// Layout is "nhwc" or "nchw". WD14 needs NHWC; joytag and Camie need
	// NCHW.
	Layout string `json:"layout,omitempty"`

	// Channels is "rgb" or "bgr". WD14 wants BGR (legacy TF training
	// data); the rest of the world ships RGB.
	Channels string `json:"channels,omitempty"`

	// Normalize selects the per-channel post-decode transform:
	//   "none"     - keep 0..255 byte values as float32
	//   "div255"   - rescale to 0..1 (rarely seen alone)
	//   "imagenet" - (x/255 - mean) / std with ImageNet statistics
	//   "clip"     - same shape as imagenet but with CLIP statistics
	Normalize string `json:"normalize,omitempty"`

	// Pad selects the resize/pad strategy:
	//   "white_square"      - pad to square with #FFFFFF then resize
	//   "mean_color_aspect" - resize preserving aspect into a square
	//                         canvas filled with FillColor (default
	//                         (124,116,104) - Camie's documented value)
	Pad string `json:"pad,omitempty"`

	// FillColor is the RGB triplet the "mean_color_aspect" pad fills the
	// background with. Ignored for other pad modes; defaults to
	// (124,116,104) when empty in the resolved profile.
	FillColor [3]uint8 `json:"fill_color,omitempty"`

	// Activation is "sigmoid_in_model" (WD14: outputs are already
	// probabilities in 0..1) or "logits" (joytag/Camie: monbooru applies
	// sigmoid before thresholding).
	Activation string `json:"activation,omitempty"`

	// LabelFormat selects the label-file parser:
	//   "wd14_csv"   - WD14 selected_tags.csv (id,name,category_id)
	//   "joytag_txt" - one label per line, all in `general`
	//   "camie_json" - Camie metadata JSON (idx_to_tag + tag_to_category)
	LabelFormat string `json:"label_format,omitempty"`

	// CategoryScheme drives processOne's per-label category resolution:
	//   "wd14_numeric"   - look label.categoryID up in wd14Category
	//   "single_general" - everything lands in general (joytag)
	//   "name_string"    - look label.categoryName up in tag_categories
	CategoryScheme string `json:"category_scheme,omitempty"`

	// OutputIndex selects which output tensor to read (0-based). Camie
	// ships a coarse + a refined head and only the second one is the
	// production output. Defaults to 0 for every other tagger.
	OutputIndex int `json:"output_index,omitempty"`
}

// fingerprint is a stable hash of the resolved profile, included in the
// taggerCache reuse signature so a sidecar edit invalidates the cached
// session set without a manual reload.
func (p Profile) fingerprint() string {
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// validate makes sure every required axis ended up populated after the
// embedded → sidecar → heuristic merge. Empty strings here mean the
// resolver couldn't fill the slot from any layer.
func (p Profile) validate() error {
	switch p.Layout {
	case "nhwc", "nchw":
	default:
		return fmt.Errorf("profile %q: bad layout %q", p.Name, p.Layout)
	}
	switch p.Channels {
	case "rgb", "bgr":
	default:
		return fmt.Errorf("profile %q: bad channels %q", p.Name, p.Channels)
	}
	switch p.Normalize {
	case "none", "div255", "imagenet", "clip":
	default:
		return fmt.Errorf("profile %q: bad normalize %q", p.Name, p.Normalize)
	}
	switch p.Pad {
	case "white_square", "mean_color_aspect":
	default:
		return fmt.Errorf("profile %q: bad pad %q", p.Name, p.Pad)
	}
	switch p.Activation {
	case "sigmoid_in_model", "logits":
	default:
		return fmt.Errorf("profile %q: bad activation %q", p.Name, p.Activation)
	}
	switch p.LabelFormat {
	case "wd14_csv", "joytag_txt", "camie_json":
	default:
		return fmt.Errorf("profile %q: bad label_format %q", p.Name, p.LabelFormat)
	}
	switch p.CategoryScheme {
	case "wd14_numeric", "single_general", "name_string":
	default:
		return fmt.Errorf("profile %q: bad category_scheme %q", p.Name, p.CategoryScheme)
	}
	return nil
}

// defaultProfileFS holds the embedded per-tagger profile defaults shipped
// with the binary (one JSON per CatalogEntry.Name under profile_default/).
// Loaded by ResolveProfile alongside the optional on-disk sidecar. The
// directory may be empty; ReadFile then returns os.ErrNotExist and
// resolution falls through to the heuristic.
//
//go:embed profile_default
var defaultProfileFS embed.FS

// wd14Profile is the heuristic fallback for `.csv` label files when no
// embedded default and no sidecar applies. Matches the pre-refactor WD14
// inference path bit-for-bit (NHWC BGR 0..255, white-square pad, sigmoid
// already in the model).
var wd14Profile = Profile{
	Layout:         "nhwc",
	Channels:       "bgr",
	Normalize:      "none",
	Pad:            "white_square",
	Activation:     "sigmoid_in_model",
	LabelFormat:    "wd14_csv",
	CategoryScheme: "wd14_numeric",
}

// joytagProfile is the heuristic fallback for `.txt` label files. Matches
// the pre-refactor joytag inference path bit-for-bit (NCHW RGB
// CLIP-normalised, white-square pad, raw logits).
var joytagProfile = Profile{
	Layout:         "nchw",
	Channels:       "rgb",
	Normalize:      "clip",
	Pad:            "white_square",
	Activation:     "logits",
	LabelFormat:    "joytag_txt",
	CategoryScheme: "single_general",
}

// ResolveProfile picks the runtime profile for one tagger.
//
// Resolution order, each layer overlaying the previous (empty string =
// no override at this key):
//  1. embedded profile_default/<taggerName>.json
//  2. <modelPath>/<taggerName>/tagger.json sidecar
//  3. heuristic from tagsFile extension (.csv → wd14, .txt → joytag)
//
// A returned error means no layer could produce a valid profile; the
// caller surfaces that as a session-open failure rather than running with
// half-resolved settings.
func ResolveProfile(modelPath, taggerName, tagsFile string) (Profile, error) {
	merged := Profile{Name: taggerName}
	if heur := heuristicProfile(tagsFile); heur != nil {
		mergeProfile(&merged, *heur)
	}
	if embedded, ok := parseEmbeddedProfile(taggerName); ok {
		mergeProfile(&merged, embedded)
	}
	if sidecar, ok := parseSidecarProfile(modelPath, taggerName); ok {
		mergeProfile(&merged, sidecar)
	}
	if err := merged.validate(); err != nil {
		return Profile{}, err
	}
	return merged, nil
}

func heuristicProfile(tagsFile string) *Profile {
	switch strings.ToLower(filepath.Ext(tagsFile)) {
	case ".csv":
		p := wd14Profile
		return &p
	case ".txt":
		p := joytagProfile
		return &p
	}
	return nil
}

// mergeProfile applies non-zero fields from src onto dst. Empty strings
// and zero ints / arrays mean "no override at this key" so a sidecar can
// flip a single axis (e.g. just OutputIndex for Camie) without restating
// the whole profile.
func mergeProfile(dst *Profile, src Profile) {
	if src.InputSize != 0 {
		dst.InputSize = src.InputSize
	}
	if src.Layout != "" {
		dst.Layout = src.Layout
	}
	if src.Channels != "" {
		dst.Channels = src.Channels
	}
	if src.Normalize != "" {
		dst.Normalize = src.Normalize
	}
	if src.Pad != "" {
		dst.Pad = src.Pad
	}
	if src.FillColor != ([3]uint8{}) {
		dst.FillColor = src.FillColor
	}
	if src.Activation != "" {
		dst.Activation = src.Activation
	}
	if src.LabelFormat != "" {
		dst.LabelFormat = src.LabelFormat
	}
	if src.CategoryScheme != "" {
		dst.CategoryScheme = src.CategoryScheme
	}
	if src.OutputIndex != 0 {
		dst.OutputIndex = src.OutputIndex
	}
}

func parseEmbeddedProfile(taggerName string) (Profile, bool) {
	data, err := defaultProfileFS.ReadFile("profile_default/" + taggerName + ".json")
	if err != nil {
		return Profile{}, false
	}
	return parseProfileDoc(data, "embedded "+taggerName)
}

func parseSidecarProfile(modelPath, taggerName string) (Profile, bool) {
	p := filepath.Join(modelPath, taggerName, "tagger.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return Profile{}, false
	}
	return parseProfileDoc(data, p)
}

type profileDoc struct {
	Version int     `json:"version"`
	Profile Profile `json:"profile"`
}

func parseProfileDoc(data []byte, source string) (Profile, bool) {
	var doc profileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		logx.Warnf("tagger: %s profile parse failed: %v", source, err)
		return Profile{}, false
	}
	if doc.Version != profileSchemaVersion {
		logx.Warnf("tagger: %s profile schema version %d unsupported (want %d)", source, doc.Version, profileSchemaVersion)
		return Profile{}, false
	}
	return doc.Profile, true
}

// EmittedCategories names the categories the profile is expected to
// produce. Used by Settings → Auto-Tagger → Configure to populate the
// per-category threshold dialog. The set is informational, not
// enforcing - a dispatch rule can route any source label into any
// category and the threshold lookup honours that at run time. nil from
// an unrecognised category scheme means "no opinion"; the caller can
// fall back to the canonical built-in list.
func (p Profile) EmittedCategories() []string {
	switch p.CategoryScheme {
	case "wd14_numeric":
		return []string{"general", "artist", "character", "copyright", "rating"}
	case "single_general":
		return []string{"general"}
	case "name_string":
		// Camie's metadata file declares one category per label and the
		// shipped model can land tags in any of these seven; surfacing
		// the full set lets operators tune each threshold up front
		// without having to first run the model and then add overrides.
		return []string{"artist", "character", "copyright", "general", "meta", "rating", "year"}
	}
	return nil
}

