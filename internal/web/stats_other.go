//go:build !linux

package web

// procRSS and mountUsage are Linux-only because the project ships in a
// Linux container and the implementations rely on /proc and the Linux
// Statfs struct shape. Non-Linux builds report unavailable; the template
// hides the corresponding rows.

func procRSS() (rssBreakdown, bool) { return rssBreakdown{}, false }

func mountUsage(_ string) (mountStats, uint64, bool) { return mountStats{}, 0, false }
