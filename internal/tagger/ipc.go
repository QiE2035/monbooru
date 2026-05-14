//go:build tagger

package tagger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leqwin/monbooru/internal/logx"
)

// ipcMethod identifies the IPC call's intent so a single
// request/response struct covers Run, Status, ReleaseIdle, ReleaseAll
// without separate wire types per method.
type ipcMethod uint8

const (
	ipcMethodRun ipcMethod = iota + 1
	ipcMethodStatus
	ipcMethodReleaseIdle
	ipcMethodReleaseAll
)

// ipcRequest is the parent → child envelope. Only one of the
// method-specific fields is populated per call.
type ipcRequest struct {
	Method      ipcMethod
	Run         *RunRequest
	IdleAfter   time.Duration
}

// ipcResponse is the child → parent envelope. Errors travel as a
// non-empty Err string so the response decode never fails on a normal
// failure. Stream=true marks a non-terminal progress frame that
// carries WorkerIdx + Msg; the receiver keeps reading until a frame
// with Stream=false arrives.
type ipcResponse struct {
	Stream    bool
	WorkerIdx int
	Msg       string
	Run       *RunResponse
	Status    *CacheStatus
	Released  bool
	Err       string
}

// writeFrame encodes payload as gob, prefixes a uint32 length, and
// writes both atomically. Returns the count of bytes the caller has
// successfully placed on the wire so partial-write errors can
// distinguish "the peer never saw this" from "the peer saw a partial
// payload".
func writeFrame(w io.Writer, payload any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return fmt.Errorf("gob encode: %w", err)
	}
	body := buf.Bytes()
	if len(body) > int(^uint32(0)) {
		return fmt.Errorf("ipc frame too large: %d bytes", len(body))
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// readFrame reads a uint32 length followed by that many bytes of gob,
// decoding into dst. Returns io.EOF when the peer closed cleanly
// before sending another frame.
func readFrame(r io.Reader, dst any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("read frame body: %w", err)
	}
	return gob.NewDecoder(bytes.NewReader(body)).Decode(dst)
}

// ipcBackend is the parent-side Backend that talks to a child
// monbooru process over a Unix-domain socket. It supervises the
// child: spawn on demand, terminate on ReleaseAll / ReleaseIdle, log
// abnormal exits, restart on the next Run.
//
// The child runs the same monbooru binary with the
// `tagger-worker --socket=<path>` argv tail; on exit (SIGTERM or
// crash) the kernel reclaims its CUDA libraries and primary context,
// which is the whole point of the subprocess split.
type ipcBackend struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	conn       net.Conn
	socketPath string
	// inFlight gates Run vs supervisor teardown so a SIGTERM never
	// races a request in progress.
	inFlight atomic.Int32
	// childPID mirrors b.cmd.Process.Pid for callers that can't take
	// b.mu (Stats panel hits WorkerPID mid-batch; the IPC streaming
	// loop holds the mutex for the whole run).
	childPID atomic.Int32
	// runUseCUDA records the in-flight Run's UseCUDA flag so a
	// Status call mid-batch reports the right mode even when the
	// cached snapshot is stale (e.g. just after a use_cuda toggle
	// where the previous snapshot is from the CPU session).
	runUseCUDA atomic.Bool
	// lastStatus caches the most recent Status response so a Status
	// call during an in-flight Run can serve from cache instead of
	// queueing behind the long-running IPC frame loop.
	lastStatus atomic.Pointer[CacheStatus]
}

// newIPCBackend constructs the parent-side IPC backend. The child is
// spawned lazily on the first Run; constructor errors are limited to
// process-environment problems (cannot determine own path).
func newIPCBackend() (*ipcBackend, error) {
	return &ipcBackend{}, nil
}

// ensureRunning starts the child if it isn't alive yet. Caller must
// hold b.mu.
func (b *ipcBackend) ensureRunning() error {
	if b.cmd != nil && b.conn != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "monbooru-tagger-*")
	if err != nil {
		return fmt.Errorf("tagger-worker socket dir: %w", err)
	}
	sockPath := filepath.Join(dir, "ipc.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("listen %q: %w", sockPath, err)
	}
	defer listener.Close()

	exe, err := os.Executable()
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("resolve own executable: %w", err)
	}
	cmd := exec.Command(exe, "tagger-worker", "--socket="+sockPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("spawn tagger-worker: %w", err)
	}
	logx.Infof("tagger-worker: spawned pid=%d socket=%s", cmd.Process.Pid, sockPath)

	// Accept the child's connection with a bounded wait so a child
	// that crashes before connecting (e.g. missing libonnxruntime)
	// surfaces a clear error instead of hanging the parent.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		c, err := listener.Accept()
		accepted <- acceptResult{c, err}
	}()

	select {
	case res := <-accepted:
		if res.err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			os.RemoveAll(dir)
			return fmt.Errorf("accept tagger-worker connection: %w", res.err)
		}
		b.cmd = cmd
		b.conn = res.conn
		b.socketPath = dir
		b.childPID.Store(int32(cmd.Process.Pid))
		return nil
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		os.RemoveAll(dir)
		return errors.New("tagger-worker did not connect within 15s")
	}
}

// terminate sends SIGTERM to the child and cleans up the socket dir.
// Caller must hold b.mu.
func (b *ipcBackend) terminate() {
	if b.cmd == nil {
		return
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(os.Interrupt)
		// Give the child a moment to exit cleanly; if it doesn't,
		// SIGKILL.
		done := make(chan struct{})
		go func() {
			b.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			b.cmd.Process.Kill()
			<-done
		}
	}
	if b.socketPath != "" {
		os.RemoveAll(b.socketPath)
		b.socketPath = ""
	}
	pid := 0
	if b.cmd.ProcessState != nil {
		pid = b.cmd.ProcessState.Pid()
	}
	b.cmd = nil
	b.childPID.Store(0)
	b.lastStatus.Store(nil)
	logx.Infof("tagger-worker: terminated pid=%d", pid)
}

// call sends a request and reads the terminal response. Caller must
// hold b.mu. A watcher goroutine closes b.conn when ctx fires so a
// wedged worker can't hang the parent indefinitely; the resulting
// wire error runs terminate() which SIGTERMs the child. The caller
// gets ctx.Err on cancellation rather than the wrapped read error.
// Intermediate Stream=true frames are forwarded to onProgress (when
// non-nil); the loop returns on the first Stream=false frame.
func (b *ipcBackend) call(ctx context.Context, req ipcRequest, onProgress func(int, string)) (ipcResponse, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if conn := b.conn; conn != nil {
				conn.Close()
			}
		case <-done:
		}
	}()

	if err := writeFrame(b.conn, req); err != nil {
		b.terminate()
		if ctx.Err() != nil {
			return ipcResponse{}, ctx.Err()
		}
		return ipcResponse{}, fmt.Errorf("ipc write: %w", err)
	}
	for {
		var resp ipcResponse
		if err := readFrame(b.conn, &resp); err != nil {
			b.terminate()
			if ctx.Err() != nil {
				return ipcResponse{}, ctx.Err()
			}
			return ipcResponse{}, fmt.Errorf("ipc read: %w", err)
		}
		if resp.Stream {
			if onProgress != nil {
				onProgress(resp.WorkerIdx, resp.Msg)
			}
			continue
		}
		return resp, nil
	}
}

// shortCallTimeout bounds Status / ReleaseIdle / ReleaseAll: long
// enough that a healthy worker's response (microseconds for Status,
// up to a few seconds for a teardown that includes mallocTrim) never
// trips it, short enough that a wedged worker doesn't hang the
// reclaim ticker or server shutdown.
const shortCallTimeout = 30 * time.Second

// Run sends the batch to the child, forwards every Stream=true
// progress frame it emits through req.OnProgress, and returns the
// terminal response. ctx cancellation unwedges the IPC reader and
// returns ctx.Err to the caller; the next Run respawns.
func (b *ipcBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	// gob can't encode the OnProgress func, so we strip it from the
	// wire payload and forward progress over the response stream
	// instead.
	wire := req
	wire.OnProgress = nil
	b.inFlight.Add(1)
	b.runUseCUDA.Store(req.UseCUDA)
	defer b.inFlight.Add(-1)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureRunning(); err != nil {
		return RunResponse{}, err
	}

	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodRun, Run: &wire}, req.OnProgress)
	if err != nil {
		return RunResponse{}, err
	}
	if resp.Err != "" {
		return RunResponse{}, errors.New(resp.Err)
	}
	if resp.Run == nil {
		return RunResponse{}, errors.New("tagger-worker returned empty response")
	}
	return *resp.Run, nil
}

// Status returns the child's cache state. While a Run is in flight,
// b.mu is held by the streaming call loop and an IPC round-trip would
// queue behind the whole batch. Short-circuit on inFlight > 0 with the
// last cached snapshot, overlaying InUse=true, so the Stats panel
// renders without waiting on the worker.
func (b *ipcBackend) Status() CacheStatus {
	if b.inFlight.Load() > 0 {
		useCUDA := b.runUseCUDA.Load()
		if cached := b.lastStatus.Load(); cached != nil {
			snap := *cached
			snap.InUse = true
			snap.UseCUDA = useCUDA
			return snap
		}
		return CacheStatus{Loaded: true, InUse: true, UseCUDA: useCUDA}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return CacheStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodStatus}, nil)
	if err != nil {
		return CacheStatus{}
	}
	if resp.Status == nil {
		return CacheStatus{}
	}
	snap := *resp.Status
	b.lastStatus.Store(&snap)
	return snap
}

// ReleaseIdle asks the child to teardown if it has been idle long
// enough. When the child reports it tore down, parent SIGTERMs to
// reclaim the CUDA libraries it loaded; the next Run respawns it.
func (b *ipcBackend) ReleaseIdle(after time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodReleaseIdle, IdleAfter: after}, nil)
	if err != nil {
		return false
	}
	if resp.Released {
		b.terminate()
	}
	return resp.Released
}

// ReleaseAll terminates the child immediately. The next Run respawns.
func (b *ipcBackend) ReleaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.terminate()
}

// WorkerPID returns the live child's PID, or (0, false) when no
// worker is currently running. Reads childPID atomically so the Stats
// panel can sample /proc/<pid>/smaps even while an autotag batch
// holds b.mu in the streaming Run loop.
func (b *ipcBackend) WorkerPID() (int, bool) {
	pid := b.childPID.Load()
	if pid == 0 {
		return 0, false
	}
	return int(pid), true
}
