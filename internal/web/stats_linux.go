//go:build linux

package web

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readVmRSS returns this process's VmRSS in bytes, or 0 on parse
// failure. Used for cheap before/after RSS deltas in log lines and
// job completion summaries without re-walking smaps.
func readVmRSS() uint64 { return readSelfStatusKB("VmRSS:") }

// readVmHWM returns the kernel's high-water mark for resident set
// size, the peak VmRSS observed for this process since start. Useful
// for honest "peak during run" reporting without sampling RSS in a
// loop. Returns 0 on parse failure.
func readVmHWM() uint64 { return readSelfStatusKB("VmHWM:") }

// readSelfStatusKB returns the first /proc/self/status line whose key
// matches prefix, parsed via parseStatusKB. 0 on absence or parse
// failure.
func readSelfStatusKB(prefix string) uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			return parseStatusKB(line[len(prefix):])
		}
	}
	return 0
}

// procRSSAt is the per-pid version of procRSS: walks /proc/<pid>/smaps
// so the worker row in the stats panel reports the same Pss-based total
// the parent does. Pss attributes each shared page at 1/N across its
// mappings, so a 1 GB inference checkpoint paged into both the worker
// and the parent through a common file shows up at its real cost in
// each row rather than double-counted under Rss. Falls back to
// /proc/<pid>/status's VmRSS / RssAnon / RssFile triplet only when the
// smaps walk itself fails; ok=false when the pid is gone or unreadable.
func procRSSAt(pid int) (rssBreakdown, bool) {
	procDir := fmt.Sprintf("/proc/%d", pid)
	if total, anon, file, db, ok := sumSmapsPssAt(procDir); ok {
		return rssBreakdown{total: total, anon: anon, file: file, db: db}, true
	}
	return readStatusRssBreakdown(procDir)
}

// readStatusRssBreakdown pulls VmRSS / RssAnon / RssFile out of
// <procDir>/status. (zero, false) when unreadable or VmRSS is missing.
func readStatusRssBreakdown(procDir string) (rssBreakdown, bool) {
	f, err := os.Open(procDir + "/status")
	if err != nil {
		return rssBreakdown{}, false
	}
	defer f.Close()
	var out rssBreakdown
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			out.total = parseStatusKB(line[len("VmRSS:"):])
		case strings.HasPrefix(line, "RssAnon:"):
			out.anon = parseStatusKB(line[len("RssAnon:"):])
		case strings.HasPrefix(line, "RssFile:"):
			out.file = parseStatusKB(line[len("RssFile:"):])
		}
	}
	if out.total == 0 {
		return rssBreakdown{}, false
	}
	return out, true
}

// procRSS reports resident set size and its breakdown in bytes. The
// totals come from /proc/self/smaps as Pss sums rather than the
// per-PTE Rss in /proc/self/status: SQLite opens 8 read + 1 write
// connections per gallery, each with its own mmap window over the same
// DB file (per-connection mmap_size cap, not per-DB), and Rss
// double-counts a shared physical page once per VMA. Pss attributes
// each shared page at 1/N across the mappings, so the tree matches
// real residency. RssAnon from /proc/self/status is used directly:
// anonymous pages are not shared between this process's VMAs, so its
// Pss equals its Rss. ok=false if VmRSS is missing or unparseable.
func procRSS() (rssBreakdown, bool) {
	out, ok := readStatusRssBreakdown("/proc/self")
	if !ok {
		return rssBreakdown{}, false
	}
	if total, _, file, db, ok := sumSmapsPssAt("/proc/self"); ok {
		out.total = total
		out.file = file
		out.db = db
	}
	return out, true
}

// sumSmapsPssAt walks <procDir>/smaps once and accumulates Pss into
// four buckets: every mapping (totalPss), anonymous (no path; named
// [heap]/[stack]/etc.; anonPss), file-backed mappings whose path is a
// real filesystem path (filePss), and the SQLite triplet *.db /
// .db-wal / .db-shm subset of filePss (dbPss). procDir is "/proc/self"
// for the main process or "/proc/<pid>" for the tagger-worker child.
// ok=false only when smaps itself is unreadable.
func sumSmapsPssAt(procDir string) (totalPss, anonPss, filePss, dbPss uint64, ok bool) {
	f, err := os.Open(procDir + "/smaps")
	if err != nil {
		return 0, 0, 0, 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var inFile, inDB bool
	for scanner.Scan() {
		line := scanner.Text()
		if isSmapsHeader(line) {
			path := extractSmapsPath(line)
			inFile = path != "" && !strings.HasPrefix(path, "[")
			inDB = inFile && isDBPath(path)
			continue
		}
		if strings.HasPrefix(line, "Pss:") {
			v := parseStatusKB(line[len("Pss:"):])
			totalPss += v
			if inFile {
				filePss += v
				if inDB {
					dbPss += v
				}
			} else {
				anonPss += v
			}
		}
	}
	return totalPss, anonPss, filePss, dbPss, true
}

// isSmapsHeader distinguishes the per-mapping header lines (which
// start with a hex address range) from the Key:value data lines. The
// first byte of a header is a hex digit; data lines start with an
// alphabetic Key.
func isSmapsHeader(line string) bool {
	if line == "" {
		return false
	}
	c := line[0]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// extractSmapsPath returns the path field of an smaps header. The
// header layout is "ADDR-ADDR perms offset dev inode  path"; the path
// is optional (anonymous mappings) and may contain spaces (re-join
// every field past the inode).
func extractSmapsPath(header string) string {
	fields := strings.Fields(header)
	if len(fields) < 6 {
		return ""
	}
	return strings.Join(fields[5:], " ")
}

func isDBPath(p string) bool {
	return strings.HasSuffix(p, ".db") ||
		strings.HasSuffix(p, ".db-wal") ||
		strings.HasSuffix(p, ".db-shm")
}

// parseStatusKB pulls the number off a `Field:    1234 kB` line and
// returns it in bytes. Returns 0 on any parse failure - the caller
// treats missing values as zero rather than propagating an error.
func parseStatusKB(rest string) uint64 {
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// mountUsage runs Statfs(path) and returns total/free/used/used%, plus a
// platform-specific filesystem id that gatherStats uses to dedup probes
// pointing at the same underlying filesystem. ok=false when the path is
// missing or Statfs fails.
func mountUsage(path string) (mountStats, uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return mountStats{}, 0, false
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	// Bavail is "blocks free for non-root" - the number `df` reports as
	// available, and the one a regular user would see at the rate of the
	// gallery filling up. Bfree includes reserved space and would
	// overstate.
	free := st.Bavail * bsize
	used := uint64(0)
	if total > free {
		used = total - free
	}
	pct := 0
	if total > 0 {
		pct = int((used * 100) / total)
	}
	// fsid is two ints32 on Linux; pack into uint64 for a stable map key.
	fsid := uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
	return mountStats{
		TotalSize: int64(total),
		FreeSize:  int64(free),
		UsedSize:  int64(used),
		UsedPct:   pct,
	}, fsid, true
}
