//go:build tagger

package tagger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/monbooru/monbooru/internal/logx"
)

// RunWorkerServer dials the parent's Unix-domain socket and
// dispatches every framed request that arrives back to the in-process
// backend until the parent closes the connection. Designed as the
// body of the `monbooru tagger-worker` subcommand.
func RunWorkerServer(ctx context.Context, socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("tagger-worker: dial %q: %w", socketPath, err)
	}
	defer conn.Close()
	logx.Infof("tagger-worker: connected to parent socket=%s", socketPath)
	return serveRequests(ctx, conn)
}

// serveRequests reads framed ipcRequests off conn and dispatches each
// to the local backend. Most methods produce one response frame; Run
// streams progress frames before its terminal frame. Returns when the
// parent closes its end (clean shutdown) or when a wire error makes
// the channel useless.
func serveRequests(ctx context.Context, conn net.Conn) error {
	// writeMu guards every conn write so Run's progress goroutine
	// can't interleave bytes with another writer. Only Run currently
	// writes outside the main loop, but the mutex is the contract
	// for any future streaming method.
	var writeMu sync.Mutex
	for {
		var req ipcRequest
		if err := readFrame(conn, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("tagger-worker: read: %w", err)
		}
		if err := handle(ctx, req, conn, &writeMu); err != nil {
			return err
		}
	}
}

// handle routes one request to the local backend, writes one or more
// response frames, and returns once the terminal frame has been
// flushed.
func handle(ctx context.Context, req ipcRequest, conn net.Conn, writeMu *sync.Mutex) error {
	switch req.Method {
	case ipcMethodRun:
		return handleRun(ctx, req, conn, writeMu)
	case ipcMethodStatus:
		st := defaultBackend.Status()
		return writeLocked(conn, writeMu, ipcResponse{Status: &st})
	case ipcMethodReleaseIdle:
		released := defaultBackend.ReleaseIdle(req.IdleAfter)
		return writeLocked(conn, writeMu, ipcResponse{Released: released})
	case ipcMethodReleaseAll:
		defaultBackend.ReleaseAll()
		return writeLocked(conn, writeMu, ipcResponse{})
	}
	return writeLocked(conn, writeMu, ipcResponse{Err: fmt.Sprintf("unknown method %d", req.Method)})
}

// handleRun installs an OnProgress that streams Stream=true frames
// back to the parent before the terminal response. Frame writes are
// serialised on writeMu so concurrent OnProgress calls from worker
// goroutines inside defaultBackend.Run never interleave on the
// socket.
func handleRun(ctx context.Context, req ipcRequest, conn net.Conn, writeMu *sync.Mutex) error {
	if req.Run == nil {
		return writeLocked(conn, writeMu, ipcResponse{Err: "run: empty request"})
	}
	runReq := *req.Run
	runReq.OnProgress = func(workerIdx int, msg string) {
		_ = writeLocked(conn, writeMu, ipcResponse{Stream: true, WorkerIdx: workerIdx, Msg: msg})
	}
	resp, err := defaultBackend.Run(ctx, runReq)
	if err != nil {
		return writeLocked(conn, writeMu, ipcResponse{Err: err.Error()})
	}
	return writeLocked(conn, writeMu, ipcResponse{Run: &resp})
}

// writeLocked is a tiny wrapper that serialises one frame write
// against any other concurrent writer on the same conn.
func writeLocked(conn net.Conn, writeMu *sync.Mutex, resp ipcResponse) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return writeFrame(conn, resp)
}
