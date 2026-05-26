package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/leqwin/monbooru/internal/jobs"
)

// startJob attempts to register a foreground job of the given type. On
// conflict it writes 409 + the standard inline flash and returns false;
// the caller should `return`. On success it returns true and the caller
// owns the goroutine + the eventual 202 response.
func (s *Server) startJob(w http.ResponseWriter, jobType string) bool {
	if err := s.jobs.Start(jobType); err != nil {
		w.WriteHeader(http.StatusConflict)
		writeInlineFlash(w, "err", "A job is already running.")
		return false
	}
	return true
}

// chunkedJob runs op on consecutive chunks of ids, honoring ctx
// cancellation between chunks and emitting jobs.Update progress with
// the noun template ("deleting", "applying implication", ...). The
// returned processed count is the number of ids reached when the loop
// exits (cancelled or completed). cancelled is true when ctx tripped
// before the slice ran out.
func chunkedJob(ctx context.Context, mgr *jobs.Manager, ids []int64, chunkSize int, noun string,
	op func(chunk []int64) error,
) (processed int, cancelled bool, err error) {
	total := len(ids)
	if mgr != nil {
		mgr.Update(0, total, fmt.Sprintf("%s 0/%d…", noun, total))
	}
	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			return processed, true, nil
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		if err := op(chunk); err != nil {
			return processed, false, err
		}
		processed = end
		if mgr != nil {
			mgr.Update(processed, total, fmt.Sprintf("%s %d/%d…", noun, processed, total))
		}
	}
	return processed, false, nil
}
