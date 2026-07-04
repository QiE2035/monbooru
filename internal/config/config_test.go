package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromValidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"

[server]
bind_address = "0.0.0.0:9090"
base_url = "http://example.com"
monloader_url = "http://localhost:8081"

[monloader]
api_url = "http://monloader:8081"

[paths]
data_path  = "/my/data"
model_path = "/my/models"

[gallery]
watch_enabled    = false
max_file_size_mb = 100

[[tagger.taggers]]
name = "wd-swinv2"
enabled = true
confidence_threshold = 0.50

[auth]
enable_password       = true
password_hash         = "$2a$10$test"
session_lifetime_days = 14
api_token             = "mysecret"

[ui]
page_size = 60

[log]
level = "debug"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.BindAddress != "0.0.0.0:9090" {
		t.Errorf("BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.Server.MonloaderURL != "http://localhost:8081" {
		t.Errorf("MonloaderURL = %q", cfg.Server.MonloaderURL)
	}
	if cfg.Monloader.APIURL != "http://monloader:8081" {
		t.Errorf("Monloader.APIURL = %q", cfg.Monloader.APIURL)
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].GalleryPath != "/my/gallery" {
		t.Errorf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.Galleries[0].DBPath != "/my/data/default/monbooru.db" {
		t.Errorf("DBPath = %q", cfg.Galleries[0].DBPath)
	}
	if cfg.Galleries[0].ThumbnailsPath != "/my/data/default/thumbnails" {
		t.Errorf("ThumbnailsPath = %q", cfg.Galleries[0].ThumbnailsPath)
	}
	if cfg.Paths.DataPath != "/my/data" {
		t.Errorf("DataPath = %q", cfg.Paths.DataPath)
	}
	if cfg.Gallery.MaxFileSizeMB != 100 {
		t.Errorf("MaxFileSizeMB = %d", cfg.Gallery.MaxFileSizeMB)
	}
	if len(cfg.Tagger.Taggers) != 1 || cfg.Tagger.Taggers[0].ConfidenceThreshold != 0.50 {
		t.Errorf("Taggers = %+v", cfg.Tagger.Taggers)
	}
	if !cfg.Auth.EnablePassword {
		t.Errorf("EnablePassword should be true")
	}
	if cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d", cfg.UI.PageSize)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q", cfg.Log.Level)
	}
}

func TestDefaultLogLevel(t *testing.T) {
	if got := Default().Log.Level; got != "warn" {
		t.Errorf("default Log.Level = %q, want %q", got, "warn")
	}
}

func TestMissingTOMLCreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Server.BindAddress != "127.0.0.1:8080" {
		t.Errorf("default BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.UI.PageSize != 40 {
		t.Errorf("default PageSize = %d", cfg.UI.PageSize)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("default config file was not created")
	}
}

func TestInvalidBindAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[server]
bind_address = "notavalidaddress"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Errorf("expected error for invalid bind address")
	}
}

func TestSessionLifetimeDaysClampsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"

[auth]
session_lifetime_days = 0
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Auth.SessionLifetimeDays != 7 {
		t.Errorf("SessionLifetimeDays = %d, want 7 (clamped)", cfg.Auth.SessionLifetimeDays)
	}
}

func TestPageSizeClampsAboveMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"

[ui]
page_size = 50000
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.UI.PageSize != MaxPageSize {
		t.Errorf("PageSize = %d, want %d (clamped)", cfg.UI.PageSize, MaxPageSize)
	}
}

func TestEnvOverrideSessionLifetimeRevalidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"

[auth]
session_lifetime_days = 30
`), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "0")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Auth.SessionLifetimeDays != 7 {
		t.Errorf("env=0 SessionLifetimeDays = %d, want 7 (clamped after override)", cfg.Auth.SessionLifetimeDays)
	}

	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "notanint")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Auth.SessionLifetimeDays != 30 {
		t.Errorf("unparseable env SessionLifetimeDays = %d, want 30 (kept)", cfg.Auth.SessionLifetimeDays)
	}
}

func TestThumbnailFitDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"absent defaults to natural", "", "natural"},
		{"bogus snaps to natural", `thumbnail_fit = "circle"`, "natural"},
		{"square preserved", `thumbnail_fit = "square"`, "square"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "monbooru.toml")
			toml := "\n[[galleries]]\nname = \"default\"\ngallery_path = \"/gallery\"\n\n[paths]\ndata_path = \"/data\"\n\n[ui]\n" + c.line + "\n"
			if err := os.WriteFile(path, []byte(toml), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if cfg.UI.ThumbnailFit != c.want {
				t.Errorf("ThumbnailFit = %q, want %q", cfg.UI.ThumbnailFit, c.want)
			}
		})
	}
}

func TestPasswordHashRejectsNonBcrypt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[[galleries]]
name = "default"
gallery_path = "/gallery"

[paths]
data_path = "/data"

[auth]
enable_password = true
password_hash   = "not-a-bcrypt-hash"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "bcrypt") {
		t.Errorf("expected bcrypt-shape error, got %v", err)
	}
}

func TestDefault_AllFields(t *testing.T) {
	cfg := Default()
	if cfg.Server.BindAddress == "" {
		t.Error("default BindAddress empty")
	}
	if cfg.Paths.DataPath == "" {
		t.Error("default DataPath empty")
	}
	if cfg.Gallery.MaxFileSizeMB <= 0 {
		t.Error("default MaxFileSizeMB <= 0")
	}
	if cfg.UI.PageSize <= 0 {
		t.Error("default PageSize <= 0")
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].Name != "default" {
		t.Errorf("default Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "default" {
		t.Errorf("default DefaultGallery = %q", cfg.DefaultGallery)
	}
}

func TestLoadMultipleGalleries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	content := `
default_gallery = "stock"

[[galleries]]
name = "default"
gallery_path = "/gallery"

[[galleries]]
name = "stock"
gallery_path = "/gallery2"

[server]
bind_address = "127.0.0.1:8080"

[paths]
data_path  = "/data"
model_path = "/models"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Galleries) != 2 {
		t.Fatalf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "stock" {
		t.Errorf("DefaultGallery = %q", cfg.DefaultGallery)
	}
	stock := cfg.FindGallery("stock")
	if stock == nil || stock.DBPath != "/data/stock/monbooru.db" {
		t.Errorf("stock DBPath = %q", stock)
	}
}

func TestDefaultGalleryFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
default_gallery = "missing"

[[galleries]]
name = "only"
gallery_path = "/gallery"
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.DefaultGallery != "only" {
		t.Errorf("DefaultGallery should fall back to only gallery, got %q", cfg.DefaultGallery)
	}
}

func TestTaggerGalleriesNilVsEmptyRoundTrip(t *testing.T) {
	// Three persisted shapes must survive Save → Load:
	//   - field absent      → nil   (legacy: applies to every gallery)
	//   - galleries = []    → []    (no gallery)
	//   - galleries = [...] → those (named subset)
	// The encoder must not collapse nil and empty into the same wire
	// shape, otherwise the "no galleries" choice degenerates back into
	// "every gallery" silently.
	cases := []struct {
		name    string
		input   []string
		applies bool
	}{
		{name: "all-nil", input: nil, applies: true},
		{name: "none", input: []string{}, applies: false},
		{name: "named", input: []string{"default"}, applies: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "monbooru.toml")
			cfg := Default()
			cfg.Tagger.Taggers = []TaggerInstance{{Name: "wd-swinv2", Enabled: true, ConfidenceThreshold: 0.4, Galleries: c.input}}
			if err := Save(cfg, path); err != nil {
				t.Fatalf("Save: %v", err)
			}
			loaded, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(loaded.Tagger.Taggers) != 1 {
				t.Fatalf("len(Taggers) = %d", len(loaded.Tagger.Taggers))
			}
			got := loaded.Tagger.Taggers[0].Galleries
			if (c.input == nil) != (got == nil) {
				t.Errorf("nil-ness drift: input nil=%t, got nil=%t (%v)", c.input == nil, got == nil, got)
			}
			if loaded.Tagger.Taggers[0].AppliesToGallery("default") != c.applies {
				t.Errorf("AppliesToGallery(default) = %t, want %t", !c.applies, c.applies)
			}
		})
	}
}

func TestLoadRejectsDuplicateGalleryNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte(`
[[galleries]]
name = "a"
gallery_path = "/gallery1"

[[galleries]]
name = "a"
gallery_path = "/gallery2"
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error for duplicate gallery name")
	}
}

func TestDerivePaths(t *testing.T) {
	cfg := Default()
	db, thumbs := cfg.DerivePaths("stock")
	if db != "/data/stock/monbooru.db" {
		t.Errorf("DerivePaths db = %q", db)
	}
	if thumbs != "/data/stock/thumbnails" {
		t.Errorf("DerivePaths thumbs = %q", thumbs)
	}
}

func TestValidateGalleryName(t *testing.T) {
	t.Parallel()
	valid := []string{"default", "a", "stock_1", "StockSet-2"}
	invalid := []string{"", "with space", "weird/slash", "dot.name", "accénté"}
	for _, n := range valid {
		n := n
		t.Run("valid/"+n, func(t *testing.T) {
			t.Parallel()
			if err := ValidateGalleryName(n); err != nil {
				t.Errorf("ValidateGalleryName(%q) = %v, want nil", n, err)
			}
		})
	}
	for _, n := range invalid {
		n := n
		t.Run("invalid/"+n, func(t *testing.T) {
			t.Parallel()
			if err := ValidateGalleryName(n); err == nil {
				t.Errorf("ValidateGalleryName(%q) = nil, want error", n)
			}
		})
	}
}

func TestSave_ErrorOnBadDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, cannot test write permission denial")
	}
	if err := Save(Default(), "/nonexistent_root/deep/path/monbooru.toml"); err == nil {
		t.Error("expected error saving to non-existent directory hierarchy")
	}
}

func TestValidate_EmptyBindAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte("[server]\nbind_address = \"\""), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error for empty bind_address")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	if err := os.WriteFile(path, []byte("not valid toml ][[["), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestSave_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skip("cannot change dir permissions")
	}
	defer func() { _ = os.Chmod(dir, 0755) }()
	if err := Save(Default(), filepath.Join(dir, "monbooru.toml")); err == nil {
		if os.Getuid() == 0 {
			t.Skip("running as root, chmod has no effect")
		}
		t.Error("expected error saving to read-only directory")
	}
}

func TestSaveAtomicTempInSameDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save failed: %v", err)
	}
	if cfg2.Server.BindAddress != cfg.Server.BindAddress {
		t.Errorf("round-trip BindAddress mismatch")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monbooru.toml.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestGenerateTokenAndLookup(t *testing.T) {
	cfg := Default()
	tok, secret := GenerateToken("ci", []string{ScopeRead})
	if len(secret) != 32 {
		t.Errorf("secret length = %d, want 32", len(secret))
	}
	if tok.TokenHash != HashToken(secret) || tok.TokenHash == secret {
		t.Error("token must store the hash, not the secret")
	}
	cfg.Auth.Tokens = append(cfg.Auth.Tokens, tok)
	if !cfg.TokenNameExists("CI") {
		t.Error("TokenNameExists should be case-insensitive")
	}
	got := cfg.FindTokenByHash(HashToken(secret))
	if got == nil || !got.HasScope(ScopeRead) || got.HasScope(ScopeWrite) {
		t.Errorf("lookup/scope mismatch: %+v", got)
	}
	if !cfg.SetTokenScopes(tok.ID, AllScopes) || !cfg.FindTokenByHash(HashToken(secret)).HasScope(ScopeDelete) {
		t.Error("SetTokenScopes did not apply")
	}
	if !cfg.RemoveToken(tok.ID) || len(cfg.Auth.Tokens) != 0 {
		t.Error("RemoveToken did not drop the token")
	}
}

func TestValidateTokenNameReserved(t *testing.T) {
	if err := ValidateTokenName("monloader"); err != nil {
		t.Errorf("plain name rejected: %v", err)
	}
	for _, bad := range []string{"", "  ", "monloader (paired)", "X (PAIRED)"} {
		if err := ValidateTokenName(bad); err == nil {
			t.Errorf("ValidateTokenName(%q) = nil, want error", bad)
		}
	}
}
