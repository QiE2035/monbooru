package tagger

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/logx"
)

//go:embed catalog_default.json
var defaultCatalogJSON []byte

// defaultDispatchFS holds the per-tagger label dispatch tables shipped
// with the binary (one JSON per CatalogEntry.Name under
// dispatch_default/). Consumed by LoadDispatch in the tagger build;
// embedded unconditionally so the data is available without a build-tag
// dance.
//
//go:embed dispatch_default/*.json
var defaultDispatchFS embed.FS

// CatalogEntry describes one downloadable tagger: a target subfolder name
// under paths.model_path plus the URLs the user fetches the model and tags
// file from. Monbooru itself never reaches out to these URLs - the Settings
// → Auto-Tagger dialog only renders copy-paste curl commands so the
// "no automatic outbound HTTP" promise stays intact.
//
// DefaultThreshold, DefaultThresholds, and DefaultTopK prefill
// TaggerInstance when an operator first enables a catalog row from
// Settings; all are optional. A non-zero DefaultThreshold replaces the
// package-wide DefaultConfidenceThreshold for that tagger;
// DefaultThresholds maps category name → per-category override and
// copies into the TaggerInstance's CategoryThresholds map; DefaultTopK
// maps category name → per-category cap and copies into the
// TaggerInstance's PerCategoryTopK map. Categories absent from
// DefaultTopK fall back to the built-in DefaultPerCategoryTopK table.
type CatalogEntry struct {
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	Files             []CatalogFile      `json:"files"`
	DefaultThreshold  float64            `json:"default_threshold,omitempty"`
	DefaultThresholds map[string]float64 `json:"default_thresholds,omitempty"`
	DefaultTopK       map[string]int     `json:"default_top_k,omitempty"`
}

// CatalogFile is one URL-to-filename pair; Filename is the basename the file
// gets dropped under inside <modelPath>/<entry name>/.
type CatalogFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

type catalogDoc struct {
	Version int            `json:"version"`
	Models  []CatalogEntry `json:"models"`
}

// LoadCatalog returns the merged tagger catalog. The embedded default
// catalog (three suggested taggers - WD14 SwinV2, JoyTag, Camie v2) is the base; an
// optional <modelPath>/models.json override is applied on top so users can
// add or replace entries without rebuilding. Same-name entries in the
// override replace the default; new names append.
func LoadCatalog(modelPath string) []CatalogEntry {
	var doc catalogDoc
	if err := json.Unmarshal(defaultCatalogJSON, &doc); err != nil {
		// Embedded asset is shipped with the binary; an invalid one is a
		// build-time bug, not a runtime concern. Surface as empty.
		return nil
	}
	out := append([]CatalogEntry(nil), doc.Models...)

	if data, err := os.ReadFile(filepath.Join(modelPath, "models.json")); err == nil {
		var override catalogDoc
		if err := json.Unmarshal(data, &override); err == nil {
			byName := map[string]int{}
			for i, e := range out {
				byName[e.Name] = i
			}
			seen := map[string]bool{}
			for _, e := range override.Models {
				if i, ok := byName[e.Name]; ok {
					// Replacing a default is intended; a second override entry
					// with the same name silently clobbers the first.
					if seen[e.Name] {
						logx.Warnf("models.json: duplicate tagger name %q; keeping the last entry", e.Name)
					}
					out[i] = e
				} else {
					byName[e.Name] = len(out)
					out = append(out, e)
				}
				seen[e.Name] = true
			}
		}
	}
	return out
}

// curlSteps builds the `mkdir -p <dir>` + per-file `curl` step list that
// downloads the entry's files into targetDir.
func (c CatalogEntry) curlSteps(targetDir string) []string {
	steps := []string{"mkdir -p " + shellSingleQuote(targetDir)}
	for _, f := range c.Files {
		dst := targetDir + "/" + f.Filename
		steps = append(steps, "curl -L -o "+shellSingleQuote(dst)+" "+shellSingleQuote(f.URL))
	}
	return steps
}

// HostCommand renders the `mkdir + curl` chain a user runs on the host
// (no docker). Paths are relative to the model path.
func (c CatalogEntry) HostCommand() string {
	return strings.Join(c.curlSteps(c.Name), " && \\\n")
}

// DockerCommand renders a `docker exec <container> sh -c '...'` chain that
// drops model files into the container's /models mount. Container name
// defaults to "monbooru" (matching the shipped docker-compose.yml).
func (c CatalogEntry) DockerCommand(containerName string) string {
	if containerName == "" {
		containerName = "monbooru"
	}
	inner := strings.Join(c.curlSteps("/models/"+c.Name), " && ")
	return fmt.Sprintf("docker exec %s sh -c %s", containerName, shellSingleQuote(inner))
}

// shellSingleQuote wraps s in shell single quotes, escaping any embedded
// single quote with the standard `'\”` recipe.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
