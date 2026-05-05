package tagger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProfile_Heuristic(t *testing.T) {
	tmp := t.TempDir() // no embedded entry, no sidecar - heuristic only
	cases := []struct {
		taggerName, tagsFile string
		wantLayout           string
		wantLabelFormat      string
	}{
		{"unknown-tagger", "tags.csv", "nhwc", "wd14_csv"},
		{"unknown-tagger", "tags.txt", "nchw", "joytag_txt"},
	}
	for _, c := range cases {
		p, err := ResolveProfile(tmp, c.taggerName, c.tagsFile)
		if err != nil {
			t.Fatalf("ResolveProfile(%q,%q): %v", c.taggerName, c.tagsFile, err)
		}
		if p.Layout != c.wantLayout {
			t.Errorf("%s: Layout = %q, want %q", c.tagsFile, p.Layout, c.wantLayout)
		}
		if p.LabelFormat != c.wantLabelFormat {
			t.Errorf("%s: LabelFormat = %q, want %q", c.tagsFile, p.LabelFormat, c.wantLabelFormat)
		}
	}
}

func TestResolveProfile_Embedded(t *testing.T) {
	// wd-swinv2 ships an embedded profile that must resolve identically
	// to the heuristic for the WD14 path - this is the bit-identical
	// invariant the catalog rows rely on.
	tmp := t.TempDir()
	got, err := ResolveProfile(tmp, "wd-swinv2", "tags.csv")
	if err != nil {
		t.Fatalf("ResolveProfile wd-swinv2: %v", err)
	}
	want := wd14Profile
	want.Name = "wd-swinv2"
	if got != want {
		t.Errorf("embedded wd-swinv2 profile drift:\n got %+v\nwant %+v", got, want)
	}

	got, err = ResolveProfile(tmp, "joytag", "tags.txt")
	if err != nil {
		t.Fatalf("ResolveProfile joytag: %v", err)
	}
	want = joytagProfile
	want.Name = "joytag"
	if got != want {
		t.Errorf("embedded joytag profile drift:\n got %+v\nwant %+v", got, want)
	}
}

func TestResolveProfile_SidecarOverridesEmbedded(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd-swinv2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tagger.json"),
		[]byte(`{"version":1,"profile":{"activation":"logits"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := ResolveProfile(tmp, "wd-swinv2", "tags.csv")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if p.Activation != "logits" {
		t.Errorf("sidecar override missed: Activation = %q, want logits", p.Activation)
	}
	// Other axes still come from the embedded default.
	if p.Layout != "nhwc" {
		t.Errorf("sidecar should not have wiped Layout: got %q", p.Layout)
	}
}

func TestResolveProfile_BadSchemaVersionFallsThrough(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd-swinv2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tagger.json"),
		[]byte(`{"version":999,"profile":{"activation":"logits"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := ResolveProfile(tmp, "wd-swinv2", "tags.csv")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	// Embedded WD14 default survives - schema mismatch dropped silently.
	if p.Activation != "sigmoid_in_model" {
		t.Errorf("schema-mismatch sidecar should not override embedded: got Activation=%q", p.Activation)
	}
}

func TestEmittedCategories(t *testing.T) {
	cases := []struct {
		scheme string
		want   []string
	}{
		{"wd14_numeric", []string{"general", "artist", "character", "copyright", "rating"}},
		{"single_general", []string{"general"}},
		{"name_string", []string{"artist", "character", "copyright", "general", "meta", "rating", "year"}},
		{"unknown", nil},
	}
	for _, c := range cases {
		got := Profile{CategoryScheme: c.scheme}.EmittedCategories()
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.scheme, got, c.want)
			continue
		}
		for i, name := range c.want {
			if got[i] != name {
				t.Errorf("%s[%d] = %q, want %q", c.scheme, i, got[i], name)
			}
		}
	}
}

func TestProfileFingerprint_Stable(t *testing.T) {
	a := wd14Profile
	a.Name = "x"
	b := wd14Profile
	b.Name = "y"
	// Name is excluded from the fingerprint via the `json:"-"` tag, so
	// two profiles that differ only in Name must hash the same.
	if a.fingerprint() != b.fingerprint() {
		t.Errorf("Name field leaks into fingerprint")
	}

	c := wd14Profile
	c.Activation = "logits"
	if a.fingerprint() == c.fingerprint() {
		t.Errorf("Activation change must change fingerprint")
	}
}
