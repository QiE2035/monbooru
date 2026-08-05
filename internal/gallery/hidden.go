// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import "strings"

// IsHiddenName reports whether path's base name starts with a dot, the
// project-wide hidden convention. The gallery root is exempted by callers.
// Both "/" and "\" count as separators so the check stays deterministic
// on Windows (where filepath.Base would miss slash-separated DB paths)
// without ever treating an empty or root path as hidden.
func IsHiddenName(path string) bool {
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "", ".", "..":
		return false
	}
	return strings.HasPrefix(base, ".")
}
