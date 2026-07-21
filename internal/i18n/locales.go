package i18n

import "embed"

// localesFS carries the embedded translation files. Files live under
// internal/i18n/locales instead of web/i18n/locales so the i18n package
// owns its data without importing the web package for assets. Every
// .toml at the embed root is read at init time and used as a
// candidate language; the active language is picked by config.
//
//go:embed locales/*.toml
var localesFS embed.FS
