//go:build !linux

package web

// procRSS and mountUsage are Linux-only because the project ships in a
// Linux container and the implementations rely on /proc and the Linux
// Statfs struct shape. Non-Linux builds report unavailable; the template
// hides the corresponding rows.

func procRSS() (rssBreakdown, bool) { return rssBreakdown{}, false }

func mountUsage(_ string) (mountStats, uint64, bool) { return mountStats{}, 0, false }

// readVmRSS has no equivalent without /proc; non-Linux builds skip the
// RSS-delta log line.
func readVmRSS() uint64 { return 0 }

// readVmHWM has no /proc equivalent; non-Linux builds report no peak.
func readVmHWM() uint64 { return 0 }

// procRSSAt is unsupported off Linux. The tagger-worker subprocess
// still runs; the stats panel just hides the worker RSS row.
func procRSSAt(_ int) (rssBreakdown, bool) { return rssBreakdown{}, false }
