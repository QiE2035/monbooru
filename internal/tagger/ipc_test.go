//go:build tagger

package tagger

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"testing"
	"time"
)

// TestFrameRoundtrip pins the wire format: gob-encoded payload behind
// a uint32 length, decoded into the matching response struct. A
// regression in either side would surface as a decode error on the
// other process.
func TestFrameRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   ipcRequest
	}{
		{
			name: "status",
			in:   ipcRequest{Method: ipcMethodStatus},
		},
		{
			name: "release_idle",
			in:   ipcRequest{Method: ipcMethodReleaseIdle, IdleAfter: 600_000_000_000}, // 10 min
		},
		{
			name: "run_with_images",
			in: ipcRequest{
				Method: ipcMethodRun,
				Run: &RunRequest{
					UseCUDA:        false,
					Parallel:       2,
					MinHitFraction: 0.05,
					Images: []BackendImageRequest{
						{ID: 1, FramePaths: []string{"/tmp/a.png"}},
						{ID: 2, FramePaths: []string{"/tmp/b.png"}, MangaProgress: true},
					},
				},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, c.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			var got ipcRequest
			if err := readFrame(&buf, &got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Method != c.in.Method {
				t.Fatalf("method: got %d, want %d", got.Method, c.in.Method)
			}
			if c.in.Run != nil {
				if got.Run == nil {
					t.Fatalf("Run round-trip lost the payload")
				}
				if got.Run.Parallel != c.in.Run.Parallel || got.Run.MinHitFraction != c.in.Run.MinHitFraction {
					t.Fatalf("scalar mismatch: got %+v, want %+v", got.Run, c.in.Run)
				}
				if len(got.Run.Images) != len(c.in.Run.Images) {
					t.Fatalf("Images: got %d, want %d", len(got.Run.Images), len(c.in.Run.Images))
				}
				for i, im := range c.in.Run.Images {
					if got.Run.Images[i].ID != im.ID || got.Run.Images[i].MangaProgress != im.MangaProgress {
						t.Errorf("Images[%d]: got %+v, want %+v", i, got.Run.Images[i], im)
					}
				}
			}
		})
	}
}

// TestRunCancelDuringInFlight pins the cancellation contract: when
// ctx fires while ipcBackend.Run is blocked reading frames from the
// worker socket, the watcher goroutine closes the conn and Run
// returns ctx.Err within milliseconds rather than hanging.
//
// The "worker" is a one-off socket pair that never writes a reply
// frame; this isolates the parent-side cancel path from the actual
// subprocess so the test stays fast and stdlib-only.
func TestRunCancelDuringInFlight(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })

	b := &ipcBackend{
		cmd:  &exec.Cmd{},
		conn: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Reader on the server side drains the request frame and
		// then never replies, simulating a long-running inference
		// in the child.
		var req ipcRequest
		_ = readFrame(server, &req)
		// Hold the socket open; the parent's Run should unblock
		// via ctx cancel, not via response.
		<-ctx.Done()
	}()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := b.Run(ctx, RunRequest{Images: []BackendImageRequest{{ID: 1, FramePaths: []string{"/tmp/x.png"}}}})
	elapsed := time.Since(start)
	if err != context.Canceled {
		t.Fatalf("Run err: got %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Run blocked for %v after cancel; expected sub-second", elapsed)
	}
}

// TestStatusReportsInFlightUseCUDA pins the contract that the Stats
// panel shows the right Mode while a Run is in flight: the cached
// snapshot may be stale (e.g. CPU from a previous session), but Status
// during inFlight overlays the current Run's UseCUDA flag.
func TestStatusReportsInFlightUseCUDA(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })

	b := &ipcBackend{
		cmd:  &exec.Cmd{},
		conn: client,
	}
	// Seed lastStatus with a stale CPU snapshot the way a real run
	// would after a Status round-trip.
	stale := CacheStatus{Loaded: true, UseCUDA: false, Sessions: []string{"wd-swinv2"}}
	b.lastStatus.Store(&stale)

	// Simulate a CUDA Run in flight without actually running it.
	b.inFlight.Add(1)
	b.runUseCUDA.Store(true)
	defer b.inFlight.Add(-1)

	snap := b.Status()
	if !snap.Loaded {
		t.Fatalf("Loaded: got false, want true")
	}
	if !snap.UseCUDA {
		t.Fatalf("UseCUDA: got false, want true (stale snapshot must be overridden)")
	}
	if !snap.InUse {
		t.Fatalf("InUse: got false, want true")
	}
}

// TestResponseRoundtrip covers the child→parent path. The decode
// step on the parent side has to handle the nil-pointer arms of the
// response: a Status reply has Run=nil, a Run reply has Status=nil.
func TestResponseRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   ipcResponse
	}{
		{"run_ok", ipcResponse{Run: &RunResponse{Results: []BackendImageResult{{ID: 7, Tags: map[TagKey]Scored{
			{Name: "blue_eyes", CatID: 0}: {Score: 0.91, TaggerName: "wd-swinv2"},
		}}}}}},
		{"status_loaded", ipcResponse{Status: &CacheStatus{Loaded: true, UseCUDA: false, InUse: false, Sessions: []string{"wd-swinv2"}}}},
		{"release_idle_true", ipcResponse{Released: true}},
		{"error", ipcResponse{Err: "model.onnx missing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeFrame(&buf, c.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			var got ipcResponse
			if err := readFrame(&buf, &got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Err != c.in.Err {
				t.Errorf("Err mismatch: got %q, want %q", got.Err, c.in.Err)
			}
			if got.Released != c.in.Released {
				t.Errorf("Released mismatch: got %v, want %v", got.Released, c.in.Released)
			}
		})
	}
}
