// Package i18n owns the runtime translation bundle: a single go-i18n
// Bundle (process-immutable after MustInit) plus a Localizer that can
// be hot-swapped at runtime via SetLanguage. The Bundle is initialised
// once by MustInit at NewServer startup so any missing / malformed
// translation file fails fast with a clear error instead of degrading
// the UI silently. The Localizer is protected by a sync.RWMutex so
// that concurrent template renders observe a consistent pointer while
// SetLanguage replaces it under a write lock.
package i18n

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"

	"github.com/leqwin/monbooru/internal/config"
)

var (
	once        sync.Once
	bundle      *i18n.Bundle
	localizerMu sync.RWMutex
	localizer   *i18n.Localizer
	initErr     error
)

// MustInit wires the i18n bundle once per process. Safe to call from
// multiple goroutines; the second-and-later callers observe the same
// outcome (initErr set or both bundle+localizer populated). The cfg
// argument must already be validated by config.Load, so the Language
// field is non-empty.
func MustInit(cfg *config.Config) error {
	once.Do(func() {
		initErr = initBundle(cfg)
	})
	return initErr
}

func initBundle(cfg *config.Config) error {
	// Parse accepts full BCP-47 tags (en, en-US, zh-CN, zh-TW, ja-JP) so
	// we can validate the exact string the operator wrote in
	// monbooru.toml, including region subtags. ParseBase would reject
	// anything beyond a single subtag, which is the wrong contract for
	// the user's "use full language codes" requirement. The library
	// re-parses the string inside NewLocalizer below; this is just a
	// fail-fast guard.
	if _, err := language.Parse(cfg.I18n.Language); err != nil {
		return fmt.Errorf("i18n: invalid language %q: %w", cfg.I18n.Language, err)
	}
	available, err := listEmbeddedLanguages()
	if err != nil {
		return fmt.Errorf("i18n: scan locales: %w", err)
	}
	if !contains(available, cfg.I18n.Language) {
		return fmt.Errorf("i18n: language %q is not available (no locales/%s.toml); available: %v",
			cfg.I18n.Language, cfg.I18n.Language, available)
	}
	// English is the bundle's default language; the localizer chains
	// the active tag + "en" so a key only translated in en.toml still
	// resolves. NewBundle requires a non-nil tag, so we always pass
	// English (the chain root), regardless of the active language.
	bundle = i18n.NewBundle(language.Make("en"))
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	for _, lang := range available {
		path := "locales/" + lang + ".toml"
		if _, err := bundle.LoadMessageFileFS(localesFS, path); err != nil {
			return fmt.Errorf("i18n: load %s: %w", path, err)
		}
	}
	// NewLocalizer accepts language tags as strings (the library parses
	// them internally); the active tag first, then English as the
	// fallback so a missing key in the active language falls through.
	localizerMu.Lock()
	localizer = i18n.NewLocalizer(bundle, cfg.I18n.Language, "en")
	localizerMu.Unlock()
	return nil
}

// Bundle returns the process-wide bundle; nil before MustInit completes.
// Callers in template / handler hot paths should not branch on nil: the
// server is required to call MustInit before serving any page, so
// reaching a nil here is a programming error and the dereference panic
// surfaces the bug in development.
func Bundle() *i18n.Bundle { return bundle }

// Localizer returns the process-wide localizer; see Bundle() for the
// nil-before-init contract. Safe for concurrent use; the read lock
// ensures callers observe a consistent pointer even during
// SetLanguage.
func Localizer() *i18n.Localizer {
	localizerMu.RLock()
	l := localizer
	localizerMu.RUnlock()
	return l
}

// SetLanguage replaces the process-wide localizer with one that
// resolves messages in the given language first, falling back to
// English. It validates that lang is a well-formed BCP-47 tag and
// that a corresponding locales/<lang>.toml exists. The Bundle itself
// (loaded message files) is immutable; only the Localizer pointer is
// swapped under a write lock, so concurrent Localizer() callers never
// see a torn value. MustInit must have succeeded before calling
// SetLanguage.
func SetLanguage(lang string) error {
	if _, err := language.Parse(lang); err != nil {
		return fmt.Errorf("i18n: invalid language %q: %w", lang, err)
	}
	available, err := listEmbeddedLanguages()
	if err != nil {
		return fmt.Errorf("i18n: scan locales: %w", err)
	}
	if !contains(available, lang) {
		return fmt.Errorf("i18n: language %q is not available (no locales/%s.toml); available: %v",
			lang, lang, available)
	}
	localizerMu.Lock()
	localizer = i18n.NewLocalizer(bundle, lang, "en")
	localizerMu.Unlock()
	return nil
}

// AvailableLocales returns the BCP-47 language codes for every embedded
// locale, sorted alphabetically. Safe to call before MustInit because it
// only inspects the embed FS; the returned slice mirrors what the
// settings dropdown will offer, so the dropdown never lists a language
// the bundle can't actually switch to.
func AvailableLocales() ([]string, error) {
	return listEmbeddedLanguages()
}

func listEmbeddedLanguages() ([]string, error) {
	entries, err := fs.ReadDir(localesFS, "locales")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".toml"))
	}
	sort.Strings(out)
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
