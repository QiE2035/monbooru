package searchkw

import "testing"

func TestValueKnown(t *testing.T) {
	cases := []struct {
		key, val string
		want     bool
	}{
		// Closed vocabularies: recognised values pass, near-misses fail.
		{"type", "image", true},
		{"type", "animated", true},
		{"type", "video", false},
		{"type", "IMAGE", true}, // value is case-insensitive
		{"mime", "png", true},
		{"mime", "zzzz", false},
		{"fav", "true", true},
		{"fav", "maybe", false},
		{"rating", "explicit", true},
		{"rating", "spicy", false},
		{"via", "upload", true},
		{"via", "magic", false},
		{"ai", "sd", true},
		{"ai", "dalle", false},
		{"relation", "duplicate", true},
		{"relation", "bogus", false},
		// Comma unions hold only when every element is recognised.
		{"type", "image,archive", true},
		{"type", "image,video", false},
		// Range and open-input keys accept anything.
		{"width", ">=1920", true},
		{"size", "10MB", true},
		{"date", "2024-01-01", true},
		{"tagcount", "5..20", true},
		{"name", "vacation", true},
		{"folder", "trips/2024", true},
		{"source", "some booru", true},
		// Bare key with no value is not flagged.
		{"type", "", true},
	}
	for _, c := range cases {
		if got := ValueKnown(c.key, c.val); got != c.want {
			t.Errorf("ValueKnown(%q, %q) = %v, want %v", c.key, c.val, got, c.want)
		}
	}
}
