//go:build tagger

package tagger

import (
	"os"
	"strings"
	"testing"
)

func TestLoadLabelsCamieJSON_Fixture(t *testing.T) {
	f, err := os.Open("testdata/camie_v2_min.json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	got, err := loadLabelsCamieJSON(f)
	if err != nil {
		t.Fatalf("loadLabelsCamieJSON: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	cases := []struct {
		idx     int
		name    string
		catName string
	}{
		{0, "1girl", "general"},
		{1, "hatsune_miku", "character"},
		{2, "wlop", "artist"},
		{3, "neon_genesis_evangelion", "copyright"},
		{4, "explicit", "rating"},
	}
	for _, c := range cases {
		if got[c.idx].name != c.name {
			t.Errorf("idx %d name = %q, want %q", c.idx, got[c.idx].name, c.name)
		}
		if got[c.idx].categoryName != c.catName {
			t.Errorf("idx %d categoryName = %q, want %q", c.idx, got[c.idx].categoryName, c.catName)
		}
		if got[c.idx].placeholder {
			t.Errorf("idx %d unexpectedly placeholder", c.idx)
		}
	}
}

func TestLoadLabelsCamieJSON_FillsHoles(t *testing.T) {
	// idx_to_tag with a hole at 2 must produce a placeholder there so
	// the slice index keeps lining up with the model's output channels.
	body := `{
		"dataset_info": {
			"tag_mapping": {
				"idx_to_tag": {"0":"a","1":"b","3":"d"},
				"tag_to_category": {"a":"general","b":"general","d":"general"}
			}
		}
	}`
	got, err := loadLabelsCamieJSON(strings.NewReader(body))
	if err != nil {
		t.Fatalf("loadLabelsCamieJSON: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (max idx 3 + 1)", len(got))
	}
	if !got[2].placeholder {
		t.Errorf("idx 2 should be a placeholder, got %+v", got[2])
	}
	if got[2].name != "_unsupported_2" {
		t.Errorf("idx 2 name = %q, want _unsupported_2", got[2].name)
	}
}

func TestResolveProfile_CamieEmbedded(t *testing.T) {
	tmp := t.TempDir()
	got, err := ResolveProfile(tmp, "camie-v2", "camie-tagger-v2-metadata.json")
	if err != nil {
		t.Fatalf("ResolveProfile camie-v2: %v", err)
	}
	want := Profile{
		Name:           "camie-v2",
		Layout:         "nchw",
		Channels:       "rgb",
		Normalize:      "imagenet",
		Pad:            "mean_color_aspect",
		Activation:     "logits",
		LabelFormat:    "camie_json",
		CategoryScheme: "name_string",
		OutputIndex:    1,
	}
	if got != want {
		t.Errorf("camie-v2 profile drift:\n got %+v\nwant %+v", got, want)
	}
}
