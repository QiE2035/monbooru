package web

import (
	"runtime"
	"strconv"
)

// rssBreakdown is what the Linux helper returns. anon comes from
// /proc/self/status RssAnon (Pss equals Rss for unshared anon pages);
// total / file / db come from a single /proc/self/smaps walk summing
// Pss so the SQLite per-connection mmap windows over the same DB file
// contribute the unique pages once. The non-Linux stub returns the
// zero value with ok=false so the template hides the corresponding rows.
type rssBreakdown struct {
	total uint64
	anon  uint64
	file  uint64
	db    uint64
}

// memStats is the process-memory snapshot rendered in the Settings → Stats
// section. Sys/HeapAlloc come from runtime.ReadMemStats; Total is Pss
// summed across /proc/self/smaps when the platform exposes it (Linux),
// otherwise zero with Available=false so the template hides the row.
type memStats struct {
	HeapAlloc  int64
	Sys        int64
	Goroutines int
	Total      int64
	// Anon is the process-private slice (Go heap + runtime + stacks +
	// glibc arenas). The kernel can only reclaim this through swap,
	// so it's the part that puts real pressure on host RAM.
	Anon int64
	// Native is the slice of Anon that Go's runtime didn't allocate:
	// Anon - Sys, clamped at zero. Three feeders: modernc/libc
	// holding SQLite's page cache; the ONNX runtime's malloc heap
	// (model weights and session arenas) when taggers are loaded;
	// any other CGO allocation. None of these flow through
	// runtime.MemStats so they're invisible to Sys.
	Native int64
	// File is the file-backed slice (mmap'd SQLite DB pages, the
	// binary's text/rodata, shared libs). Cheap under pressure: the
	// kernel evicts it without swap, since the pages are just a cache
	// of files already on disk.
	File int64
	// DB is the slice of File attributed to *.db / -wal / -shm
	// mappings - the SQLite mmap window pages. The remainder of File
	// is the binary text + rodata + shared libs.
	DB        int64
	OtherFile int64
	Available bool
}

// galleryDBStats is one row of the per-gallery DB-size table. DBSize sums
// the SQLite triplet (.db + .db-wal + .db-shm) - WAL can dominate after a
// long write session and is part of "what's on disk for this gallery".
type galleryDBStats struct {
	Name   string
	DBPath string
	DBSize int64
}

// mountStats is one row of the filesystem free-space block. Each unique
// underlying filesystem (deduped by Statfs.fsid in mountUsage) is rendered
// once, with the labels listing every gallery path that resolves to it so
// the operator can map a row back to the galleries it serves.
type mountStats struct {
	Labels    []string
	TotalSize int64
	FreeSize  int64
	UsedSize  int64
	UsedPct   int
}

// statsData is the bundle threaded into the settings template.
type statsData struct {
	Mem        memStats
	Galleries  []galleryDBStats
	Mounts     []mountStats
	FSWarnings []string // non-empty when mountUsage failed; rendered as a hint
}

// gatherStats builds the snapshot rendered in the Stats section. Cheap by
// design: a runtime.ReadMemStats, three os.Stat per gallery, one Statfs per
// unique filesystem. No directory walks.
func (s *Server) gatherStats() statsData {
	out := statsData{Mem: gatherMemStats()}

	// Galleries: stable name order for table render. The settings page
	// already shows galleries name-sorted; matching that avoids a "two
	// orderings on one page" mismatch.
	galleries := s.galleryList()
	out.Galleries = make([]galleryDBStats, 0, len(galleries))
	for _, g := range galleries {
		out.Galleries = append(out.Galleries, galleryDBStats{
			Name:   g.Name,
			DBPath: g.DBPath,
			DBSize: dbFileSize(g.DBPath),
		})
	}

	// Mounts: probe each gallery's DB, images, and thumbnails dirs and
	// dedup by the Statfs fsid the platform helper returns. Within one
	// row a gallery is listed once even if its three dirs all resolve
	// there; across rows the same gallery may appear twice if its data
	// is split across filesystems, which is itself useful information.
	type mountAcc struct {
		galleries []string
		seenInRow map[string]bool
		stats     mountStats
	}
	byKey := map[string]*mountAcc{}
	var order []*mountAcc
	addProbe := func(galleryName, path string) {
		if path == "" {
			return
		}
		st, fsid, ok := mountUsage(path)
		if !ok {
			return
		}
		// Some filesystems (overlay, tmpfs on certain kernels) report
		// fsid zero. Key those by path so probes that obviously belong
		// to the same dir tree still merge, while distinct zero-fsid
		// volumes don't get collapsed.
		key := strconv.FormatUint(fsid, 10)
		if fsid == 0 {
			key = "path:" + path
		}
		if acc, ok := byKey[key]; ok {
			if !acc.seenInRow[galleryName] {
				acc.seenInRow[galleryName] = true
				acc.galleries = append(acc.galleries, galleryName)
			}
			return
		}
		acc := &mountAcc{
			galleries: []string{galleryName},
			seenInRow: map[string]bool{galleryName: true},
			stats:     st,
		}
		byKey[key] = acc
		order = append(order, acc)
	}
	for _, g := range galleries {
		addProbe(g.Name, g.DBPath)
		addProbe(g.Name, g.GalleryPath)
		addProbe(g.Name, g.ThumbnailsPath)
	}
	// Second-pass dedup keyed on the size tuple. Statfs sometimes
	// reports distinct fsids for the same physical filesystem (Docker
	// bind mounts and overlay layers do this on Linux), so the fsid
	// dedup above leaves the row repeated. Two filesystems reporting
	// byte-identical TotalSize/FreeSize/UsedSize triples are
	// effectively the same volume from the operator's perspective.
	type sizeKey struct{ Total, Free, Used int64 }
	bySize := map[sizeKey]int{}
	out.Mounts = make([]mountStats, 0, len(order))
	for _, m := range order {
		st := m.stats
		st.Labels = m.galleries
		key := sizeKey{st.TotalSize, st.FreeSize, st.UsedSize}
		if idx, ok := bySize[key]; ok {
			seen := map[string]bool{}
			for _, name := range out.Mounts[idx].Labels {
				seen[name] = true
			}
			for _, name := range st.Labels {
				if !seen[name] {
					seen[name] = true
					out.Mounts[idx].Labels = append(out.Mounts[idx].Labels, name)
				}
			}
			continue
		}
		bySize[key] = len(out.Mounts)
		out.Mounts = append(out.Mounts, st)
	}
	if len(out.Mounts) == 0 {
		out.FSWarnings = append(out.FSWarnings,
			"Filesystem usage unavailable on this platform.")
	}
	return out
}

// gatherMemStats reads runtime.ReadMemStats and asks procRSS for the
// per-process residency breakdown. procRSS returns ok=false on platforms
// without a cheap RSS source; the template hides the dependent rows.
func gatherMemStats() memStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := memStats{
		HeapAlloc:  int64(ms.HeapAlloc),
		Sys:        int64(ms.Sys),
		Goroutines: runtime.NumGoroutine(),
	}
	if r, ok := procRSS(); ok {
		out.Total = int64(r.total)
		out.Anon = int64(r.anon)
		out.File = int64(r.file)
		out.DB = int64(r.db)
		out.OtherFile = out.File - out.DB
		if out.OtherFile < 0 {
			out.OtherFile = 0
		}
		// Sys can briefly exceed Anon when Go reserves arenas it
		// hasn't faulted yet; clamp to zero so the row never
		// renders as a negative.
		out.Native = out.Anon - out.Sys
		if out.Native < 0 {
			out.Native = 0
		}
		out.Available = true
	}
	return out
}
