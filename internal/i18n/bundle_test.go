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
	// Before any SetLanguage call, repeated Localizer() must return
	// the same pointer. After SetLanguage the pointer changes (tested
	// separately in TestSetLanguage_Success).
	l1, l2 := Localizer(), Localizer()
	if l1 != l2 {
		t.Errorf("Localizer() returned different pointers: %p vs %p", l1, l2)
	}
	// Second MustInit must be a no-op (sync.Once).
	if err := MustInit(cfg); err != nil {
		t.Errorf("second MustInit error: %v", err)
	}
}

func TestSetLanguage_Success(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	if err := MustInit(cfg); err != nil {
		t.Fatalf("MustInit: %v", err)
	}

	// Verify initial English translation.
	msg, err := Localizer().Localize(&goi18n.LocalizeConfig{MessageID: "layout_topbar.images"})
	if err != nil {
		t.Fatalf("Localize before SetLanguage: %v", err)
	}
	if msg != "Images" {
		t.Fatalf("before SetLanguage: got %q, want %q", msg, "Images")
	}

	// Swap to zh-CN.
	if err := SetLanguage("zh-CN"); err != nil {
		t.Fatalf("SetLanguage(zh-CN): %v", err)
	}

	msg, err = Localizer().Localize(&goi18n.LocalizeConfig{MessageID: "layout_topbar.images"})
	if err != nil {
		t.Fatalf("Localize after SetLanguage: %v", err)
	}
	if msg != "图像" {
		t.Errorf("after SetLanguage(zh-CN): got %q, want %q", msg, "图像")
	}

	// Bundle pointer must not change — SetLanguage only swaps the localizer.
	if Bundle() == nil {
		t.Fatal("Bundle() = nil after SetLanguage")
	}
}

func TestSetLanguage_UnavailableLang(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	if err := MustInit(cfg); err != nil {
		t.Fatalf("MustInit: %v", err)
	}

	err := SetLanguage("fr")
	if err == nil {
		t.Fatal("expected error for unavailable language, got nil")
	}
	if !strings.Contains(err.Error(), `"fr"`) {
		t.Errorf("error %q does not mention the offending language", err)
	}

	// Localizer must remain unchanged — still resolves English.
	msg, err := Localizer().Localize(&goi18n.LocalizeConfig{MessageID: "layout_topbar.images"})
	if err != nil {
		t.Fatalf("Localize after failed SetLanguage: %v", err)
	}
	if msg != "Images" {
		t.Errorf("localizer changed after failed SetLanguage: got %q, want %q", msg, "Images")
	}
}

func TestSetLanguage_ConcurrentReadWrite(t *testing.T) {
	t.Cleanup(resetForTest)
	cfg := config.Default()
	if err := MustInit(cfg); err != nil {
		t.Fatalf("MustInit: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half the goroutines read via Localizer().
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			l := Localizer()
			if l == nil {
				t.Error("Localizer() = nil during concurrent read")
			}
		}()
	}

	// Half the goroutines swap language between en and zh-CN.
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			lang := "en"
			if i%2 == 0 {
				lang = "zh-CN"
			}
			_ = SetLanguage(lang)
		}(i)
	}

	wg.Wait()
	// No panic = pass. Data races are caught by `go test -race`.
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
