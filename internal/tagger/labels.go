package tagger

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/tags"
)

// sanitizeLabel coerces a label-file name into the stored tag form through the
// same normalizer the add path uses (tags.NormalizeName): spaces fold to
// underscores, control runes drop, the rest of Unicode is kept. A label that
// empties out, or holds no content rune, becomes `_unsupported_<idx>` so the
// slice index keeps its 1:1 mapping with the model's output channels - dropping
// the entry would shift every later label and corrupt downstream attribution.
// The returned bool is false in that fallback case so callers can flag the slot
// as a placeholder and skip emission at inference time.
func sanitizeLabel(raw string, idx int) (string, bool) {
	name := tags.NormalizeName(raw)
	if rs := []rune(name); len(rs) > 200 {
		name = string(rs[:200])
	}
	if name == "" || !tags.HasTagContent(name) {
		return fmt.Sprintf("_unsupported_%d", idx), false
	}
	return name, true
}

// tagLabel holds a parsed row from the model's label file. The slice
// index always lines up 1:1 with the model's output channels even for
// placeholder rows, so inference can index into either side without
// translation.
type tagLabel struct {
	name string
	// categoryID is the integer category code WD14-format CSVs declare.
	// Zero (general) for joytag-format `.txt` and Camie JSON.
	categoryID int
	// categoryName is the textual category Camie's JSON declares per tag
	// ("artist", "copyright", ...). Empty for WD14 / joytag.
	categoryName string
	// placeholder is true when the label-file row had no usable name
	// (e.g. only punctuation) and the slot was filled with an
	// `_unsupported_<idx>` stub. Inference must skip these slots so the
	// stub never becomes a real tag.
	placeholder bool
}

// loadLabels parses the tagger's label file according to the resolved
// profile's LabelFormat. The slice index always lines up 1:1 with the
// model's output channels, even for placeholder rows.
func loadLabels(path string, profile Profile) ([]tagLabel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	switch profile.LabelFormat {
	case "wd14_csv":
		return loadLabelsCSV(f)
	case "joytag_txt":
		return loadLabelsText(f)
	case "camie_json":
		return loadLabelsCamieJSON(f)
	}
	return nil, fmt.Errorf("loadLabels: unsupported label_format %q", profile.LabelFormat)
}

func loadLabelsCSV(f io.Reader) ([]tagLabel, error) {
	r := csv.NewReader(f)
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	var labels []tagLabel
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 3 {
			continue
		}
		catID, _ := strconv.Atoi(strings.TrimSpace(rec[2]))
		name, ok := sanitizeLabel(rec[1], len(labels))
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  catID,
			placeholder: !ok,
		})
	}
	return labels, nil
}

func loadLabelsText(f io.Reader) ([]tagLabel, error) {
	var labels []tagLabel
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name, ok := sanitizeLabel(raw, len(labels))
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  0,
			placeholder: !ok,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// camieMetadata mirrors the relevant subset of Camie's
// camie-tagger-v2-metadata.json: dataset_info.tag_mapping carries
// idx_to_tag (array indexed by output channel) and tag_to_category
// (name → category-name). Other fields in the metadata file are
// ignored.
type camieMetadata struct {
	DatasetInfo struct {
		TagMapping struct {
			IdxToTag      map[string]string `json:"idx_to_tag"`
			TagToCategory map[string]string `json:"tag_to_category"`
		} `json:"tag_mapping"`
	} `json:"dataset_info"`
}

// loadLabelsCamieJSON parses Camie's metadata file. idx_to_tag is a
// JSON object whose keys are stringified integer indices ("0", "1",
// ...); we materialise it into a slice in index order. The category
// table maps each label name to a string category, copied onto the
// resulting tagLabel so the name_string resolver can look it up at
// inference time.
func loadLabelsCamieJSON(f io.Reader) ([]tagLabel, error) {
	var doc camieMetadata
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode camie metadata: %w", err)
	}
	idxMap := doc.DatasetInfo.TagMapping.IdxToTag
	if len(idxMap) == 0 {
		return nil, fmt.Errorf("camie metadata: dataset_info.tag_mapping.idx_to_tag is empty")
	}
	maxIdx := -1
	for k := range idxMap {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 {
			return nil, fmt.Errorf("camie metadata: bad idx %q", k)
		}
		if i > maxIdx {
			maxIdx = i
		}
	}
	labels := make([]tagLabel, maxIdx+1)
	cats := doc.DatasetInfo.TagMapping.TagToCategory
	for k, raw := range idxMap {
		i, _ := strconv.Atoi(k)
		name, ok := sanitizeLabel(raw, i)
		labels[i] = tagLabel{
			name:         name,
			categoryName: cats[raw],
			placeholder:  !ok,
		}
	}
	// Any unfilled slot (idx_to_tag had a hole) becomes a placeholder so
	// the slice index stays aligned with the model's output channels.
	for i := range labels {
		if labels[i].name == "" {
			labels[i] = tagLabel{
				name:        fmt.Sprintf("_unsupported_%d", i),
				placeholder: true,
			}
		}
	}
	return labels, nil
}
