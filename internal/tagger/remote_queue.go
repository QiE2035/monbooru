//go:build tagger

package tagger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultRemoteQueueCapacity is the capacity used when no config
	// value has been applied yet.
	defaultRemoteQueueCapacity = 16
	// maxRemoteQueueCapacity clamps hand-edited or form-posted values.
	maxRemoteQueueCapacity = 64
	// remoteResultTTL is how long completed jobs and their per-token
	// FIFO entries are retained before GC. Long enough that a peer
	// that briefly drops its connection can resume draining; short
	// enough that a peer that never comes back doesn't pin memory.
	remoteResultTTL = 5 * time.Minute
	// maxPerTokenResults bounds a slow peer's retained FIFO so one
	// lagging drainer can't grow the queue's memory without bound.
	// Entries beyond the cap are dropped (oldest first) and the peer's
	// per-job timeout treats them as failed/skipped.
	maxPerTokenResults = 4096
	// batchCoalesceDelay is the window the dispatcher holds a short
	// queue to let a parallel-sized batch form. Remote submissions
	// arrive as one HTTP request per image, so without this the
	// dispatcher would tag one image per IPC call and forfeit the
	// worker pool's parallelism.
	batchCoalesceDelay = 10 * time.Millisecond
	// dispatchTimeout bounds one dispatcher batch so a wedged worker
	// can't stall the whole queue forever; the IPC watcher terminates
	// the child on cancellation and the next batch respawns it.
	dispatchTimeout = 15 * time.Minute
)

// remoteJob is one enqueued image awaiting dispatch and its eventual
// result. token routes the completed result to the submitting peer's
// drain cursor. seq is assigned in completion order and is the drain's
// ordering key.
type remoteJob struct {
	id        string
	token     string
	image     BackendImageRequest
	params    RemoteRunParams
	createdAt time.Time
	done      bool
	seq       int64
}

// resultEntry is one completed job's outcome placed on the owning
// token's FIFO, keyed by the global completion seq and stamped with
// the completion time for TTL GC.
type resultEntry struct {
	Seq    int64
	JobID  string
	At     time.Time
	Result BackendImageResult
}

// remoteTaggerQueue is the A-side submission queue. Submissions are
// accepted while queued+inflight < capacity and dispatched to the
// local backend in parallel-sized batches; completed results are
// appended to a per-token FIFO so each remote peer drains only its own
// images. A single dispatcher goroutine owns every backend.Run call so
// IPC and session reuse stay serialised like a local batch run.
type remoteTaggerQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int
	jobs     map[string]*remoteJob
	queue    []string // FIFO of queued (not yet dispatched) job ids
	inflight int
	seq      atomic.Int64
	perToken map[string][]resultEntry
	started  bool
}

// remoteQueue is the package-level singleton, mirroring defaultBackend.
var remoteQueue = &remoteTaggerQueue{}

func init() {
	remoteQueue.mu.Lock()
	remoteQueue.capacity = defaultRemoteQueueCapacity
	remoteQueue.jobs = map[string]*remoteJob{}
	remoteQueue.perToken = map[string][]resultEntry{}
	remoteQueue.cond = sync.NewCond(&remoteQueue.mu)
	remoteQueue.mu.Unlock()
}

// newRemoteJobID mints a random job identifier.
func newRemoteJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("remote job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SubmitRemoteImage enqueues one image for remote tagging. It returns
// the job id, or ErrRemoteQueueFull when the queue is at capacity.
// Tagging happens asynchronously; results are read back via
// RemoteDrainResults under the same token.
func SubmitRemoteImage(_ context.Context, params RemoteRunParams, image BackendImageRequest, token string) (string, error) {
	q := remoteQueue
	id, err := newRemoteJobID()
	if err != nil {
		return "", err
	}
	q.mu.Lock()
	if q.capacity < 1 {
		q.capacity = defaultRemoteQueueCapacity
	}
	if len(q.queue)+q.inflight >= q.capacity {
		q.mu.Unlock()
		return "", ErrRemoteQueueFull
	}
	q.jobs[id] = &remoteJob{
		id:        id,
		token:     token,
		image:     image,
		params:    params,
		createdAt: time.Now(),
	}
	q.queue = append(q.queue, id)
	q.mu.Unlock()
	q.cond.Signal()
	q.start()
	return id, nil
}

// RemoteQueueStatus returns the current capacity, queued image count,
// and in-flight image count. capacity is the queued+inflight ceiling.
func RemoteQueueStatus() (int, int, int) {
	q := remoteQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.capacity, len(q.queue), q.inflight
}

// RemoteDrainResults blocks until the given token has at least one
// completed result with seq > after, or wait elapses. It returns the
// new cursor and the matching results in ascending seq order; on
// timeout it returns the unchanged cursor with an empty slice. The
// call is idempotent: a peer that reconnects can resume from a stored
// cursor and never re-receives a result it already drained.
func RemoteDrainResults(token string, after int64, wait time.Duration) (int64, []RemoteDrainedResult, error) {
	q := remoteQueue
	q.mu.Lock()
	defer q.mu.Unlock()

	deadline := time.Now().Add(wait)
	for {
		entries := q.perToken[token]
		newCursor := after
		var out []RemoteDrainedResult
		for _, e := range entries {
			if e.Seq <= after {
				continue
			}
			out = append(out, RemoteDrainedResult{JobID: e.JobID, Tags: e.Result.Tags, Err: e.Result.Err})
			if e.Seq > newCursor {
				newCursor = e.Seq
			}
		}
		if len(out) > 0 {
			return newCursor, out, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return after, nil, nil
		}
		// sync.Cond has no timed wait, so arm a timer that broadcasts
		// on expiry and lets Wait return on timeout.
		timer := time.NewTimer(remaining)
		stopped := make(chan struct{})
		go func() {
			select {
			case <-timer.C:
				q.cond.Broadcast()
			case <-stopped:
			}
		}()
		q.cond.Wait()
		timer.Stop()
		close(stopped)
	}
}

// SetRemoteQueueCapacity updates the queue's image capacity. Values
// outside [1, maxRemoteQueueCapacity] are clamped. Called from the
// settings save path and at startup so the configured value wins.
func SetRemoteQueueCapacity(n int) {
	if n < 1 {
		n = defaultRemoteQueueCapacity
	}
	if n > maxRemoteQueueCapacity {
		n = maxRemoteQueueCapacity
	}
	q := remoteQueue
	q.mu.Lock()
	q.capacity = n
	q.mu.Unlock()
}

// start launches the dispatcher and GC goroutines once, on first use.
func (q *remoteTaggerQueue) start() {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()
	go q.dispatcherLoop()
	go q.gcLoop()
}

// dispatcherLoop pops parallel-sized batches from the FIFO and runs
// each through backend.Run, then routes the results to the owning
// tokens. It is the only goroutine that pops the queue, so batch
// ordering and inflight accounting stay race-free.
func (q *remoteTaggerQueue) dispatcherLoop() {
	for {
		q.mu.Lock()
		for len(q.queue) == 0 {
			q.cond.Wait()
		}
		first := q.jobs[q.queue[0]]
		// Coalescing window: submissions arrive one per HTTP request,
		// so wait a few ms for a parallel-sized group to form.
		if len(q.queue) < first.params.Parallel {
			q.mu.Unlock()
			time.Sleep(batchCoalesceDelay)
			q.mu.Lock()
			if len(q.queue) == 0 {
				q.mu.Unlock()
				continue
			}
			first = q.jobs[q.queue[0]]
		}
		batch := q.takeBatch(first.params)
		q.mu.Unlock()

		if len(batch) == 0 {
			continue
		}
		q.dispatch(batch)
	}
}

// takeBatch pops up to parallel jobs from the queue head that share
// the same run params as the reference, incrementing inflight for
// each. Jobs with different params are left queued for their own
// batch. Caller must hold q.mu.
func (q *remoteTaggerQueue) takeBatch(params RemoteRunParams) []*remoteJob {
	target := params.Parallel
	if target < 1 {
		target = 1
	}
	if len(q.queue) < target {
		target = len(q.queue)
	}
	batch := make([]*remoteJob, 0, target)
	for len(q.queue) > 0 && len(batch) < target {
		id := q.queue[0]
		job := q.jobs[id]
		if !sameRunParams(job.params, params) {
			break
		}
		q.queue = q.queue[1:]
		batch = append(batch, job)
	}
	q.inflight += len(batch)
	return batch
}

// dispatch runs one batch through the local backend and completes
// every job in it, assigning completion seqs and routing results to
// the per-token FIFOs. backend.Run internally splits the batch by byte
// budget (splitRuns), so a parallel-sized batch still gets the full
// worker pool while never exceeding the IPC frame cap.
func (q *remoteTaggerQueue) dispatch(batch []*remoteJob) {
	backend := activeBackend()
	if backend == nil {
		q.failBatch(batch, "auto-tagger disabled (no backend registered)")
		return
	}
	first := batch[0]
	images := make([]BackendImageRequest, 0, len(batch))
	for _, j := range batch {
		images = append(images, j.image)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()
	resp, err := backend.Run(ctx, RunRequest{
		Cfg:            first.params.Cfg,
		Taggers:        first.params.Taggers,
		Provider:       first.params.Provider,
		CatIDs:         first.params.CatIDs,
		GeneralCatID:   first.params.GeneralCatID,
		InferredCats:   first.params.InferredCats,
		MinHitFraction: first.params.MinHitFraction,
		Parallel:       first.params.Parallel,
		Images:         images,
	})
	if err != nil {
		q.failBatch(batch, err.Error())
		return
	}
	// Results are aligned with the requested image order.
	for i, j := range batch {
		if i < len(resp.Results) {
			q.completeJob(j, &resp.Results[i])
		} else {
			q.completeJob(j, &BackendImageResult{ID: j.image.ID, Err: "remote tagger returned no result"})
		}
	}
}

// failBatch completes every job in the batch as failed.
func (q *remoteTaggerQueue) failBatch(batch []*remoteJob, errMsg string) {
	for _, j := range batch {
		q.completeJob(j, &BackendImageResult{ID: j.image.ID, Err: errMsg})
	}
}

// completeJob marks a job done, assigns its completion seq, appends
// the outcome to the token's FIFO, and wakes drain waiters.
func (q *remoteTaggerQueue) completeJob(job *remoteJob, result *BackendImageResult) {
	seq := q.seq.Add(1)
	q.mu.Lock()
	defer q.mu.Unlock()
	job.done = true
	job.seq = seq
	q.inflight--
	q.perToken[job.token] = append(q.perToken[job.token], resultEntry{
		Seq:    seq,
		JobID:  job.id,
		At:     time.Now(),
		Result: *result,
	})
	q.trimToken(job.token)
	q.cond.Broadcast()
}

// trimToken drops the oldest retained results for a token once its
// FIFO exceeds maxPerTokenResults.
func (q *remoteTaggerQueue) trimToken(token string) {
	entries := q.perToken[token]
	if len(entries) > maxPerTokenResults {
		q.perToken[token] = entries[len(entries)-maxPerTokenResults:]
	}
}

// gcLoop periodically drops completed jobs and retained results past
// remoteResultTTL.
func (q *remoteTaggerQueue) gcLoop() {
	ticker := time.NewTicker(remoteResultTTL / 2)
	defer ticker.Stop()
	for range ticker.C {
		q.gc()
	}
}

func (q *remoteTaggerQueue) gc() {
	cutoff := time.Now().Add(-remoteResultTTL)
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, j := range q.jobs {
		if j.done && j.createdAt.Before(cutoff) {
			delete(q.jobs, id)
		}
	}
	for token, entries := range q.perToken {
		kept := entries[:0]
		for _, e := range entries {
			if !e.At.Before(cutoff) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(q.perToken, token)
		} else {
			q.perToken[token] = kept
		}
	}
}

// sameRunParams reports whether two parameter snapshots are identical
// enough to share one dispatcher batch. The A-side receives every job
// from the same running server, so in practice all submitted params
// match and batches stay parallel-sized; the comparison only guards
// against a config edit landing mid-queue.
func sameRunParams(a, b RemoteRunParams) bool {
	if a.Cfg != b.Cfg || a.Provider != b.Provider || a.GeneralCatID != b.GeneralCatID ||
		a.MinHitFraction != b.MinHitFraction || a.Parallel != b.Parallel {
		return false
	}
	if len(a.Taggers) != len(b.Taggers) || len(a.CatIDs) != len(b.CatIDs) || len(a.InferredCats) != len(b.InferredCats) {
		return false
	}
	for i := range a.Taggers {
		if a.Taggers[i].Name != b.Taggers[i].Name {
			return false
		}
	}
	for k, v := range a.CatIDs {
		if b.CatIDs[k] != v {
			return false
		}
	}
	for k, v := range a.InferredCats {
		if b.InferredCats[k] != v {
			return false
		}
	}
	return true
}
