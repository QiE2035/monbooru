package logx

import "testing"

// Levels are ordered low-to-high (Debug < Info < Warn) so Set("info")
// enables Info and Warn but not Debug, matching the slog convention.
func TestEnabledMatchesThreshold(t *testing.T) {
	cases := []struct {
		threshold string
		expect    map[Level]bool
	}{
		{
			threshold: "debug",
			expect: map[Level]bool{
				LevelDebug: true,
				LevelInfo:  true,
				LevelWarn:  true,
			},
		},
		{
			threshold: "info",
			expect: map[Level]bool{
				LevelDebug: false,
				LevelInfo:  true,
				LevelWarn:  true,
			},
		},
		{
			threshold: "warn",
			expect: map[Level]bool{
				LevelDebug: false,
				LevelInfo:  false,
				LevelWarn:  true,
			},
		},
	}

	for _, tc := range cases {
		Set(tc.threshold)
		for l, want := range tc.expect {
			if got := Enabled(l); got != want {
				t.Errorf("threshold=%q level=%d: Enabled = %v, want %v", tc.threshold, l, got, want)
			}
		}
	}
}
