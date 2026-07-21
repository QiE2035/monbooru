package i18n

import (
	"strings"
	"sync"
	"testing"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"

	"github.com/leqwin/monbooru/internal/config"
)

// resetForTest wipes the package-level singleton so a fresh MustInit
// call lands on a clean slate. Each test that calls MustInit must
// invoke this from t.Cleanup so test ordering can't leak state.
func resetForTest() {
	once = sync.Once{}
	bundle = nil
	localizer = nil
	initErr = nil
}

func TestMustInit_DefaultEnglish(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	if err := MustInit(cfg); err != nil {
		t.Fatalf("MustInit(en): %v", err)
	}
	if Bundle() == nil {
		t.Fatal("Bundle() = nil after MustInit")
	}
	if Localizer() == nil {
		t.Fatal("Localizer() = nil after MustInit")
	}
	// Sanity: localize a known key from en.toml; the localizer must
	// return the translated string and a nil error so templates that
	// already wire `T` work end-to-end. The bundle key is dot-separated
	// because go-i18n flattens TOML tables on load.
	msg, err := Localizer().Localize(&goi18n.LocalizeConfig{MessageID: "layout_topbar.images"})
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	if msg != "Images" {
		t.Errorf("Localize(layout_topbar.images) = %q, want %q", msg, "Images")
	}
}

func TestBundle_Singleton(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	if err := MustInit(cfg); err != nil {
		t.Fatalf("MustInit: %v", err)
	}
	b1, b2 := Bundle(), Bundle()
	if b1 != b2 {
		t.Errorf("Bundle() returned different pointers: %p vs %p", b1, b2)
	}
	l1, l2 := Localizer(), Localizer()
	if l1 != l2 {
		t.Errorf("Localizer() returned different pointers: %p vs %p", l1, l2)
	}
	// Second MustInit must be a no-op (sync.Once).
	if err := MustInit(cfg); err != nil {
		t.Errorf("second MustInit error: %v", err)
	}
}

func TestMustInit_UnknownLanguage(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	cfg.I18n.Language = "fr"
	err := MustInit(cfg)
	if err == nil {
		t.Fatal("expected error for unknown language, got nil")
	}
	if !strings.Contains(err.Error(), `"fr"`) {
		t.Errorf("error %q does not name the offending language", err)
	}
}

func TestMustInit_InvalidLanguageTag(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	cfg.I18n.Language = "not_a_real_lang_xyz_999"
	err := MustInit(cfg)
	if err == nil {
		t.Fatal("expected error for invalid language tag, got nil")
	}
}

func TestAvailableLocales_IncludesEmbedded(t *testing.T) {
	got, err := AvailableLocales()
	if err != nil {
		t.Fatalf("AvailableLocales: %v", err)
	}
	want := []string{"en", "zh-CN", "zh-TW"}
	if len(got) != len(want) {
		t.Fatalf("AvailableLocales = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("AvailableLocales[%d] = %q, want %q (full slice: %v)", i, got[i], w, got)
		}
	}
}
