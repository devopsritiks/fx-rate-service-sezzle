package audit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSink is an in-memory Sink for testing the Writer's batching,
// retry, and shutdown-flush behavior without a real Postgres.
type fakeSink struct {
	mu      sync.Mutex
	batches [][]Record

	// failNextN causes the next N WriteBatch calls to return failErr
	// before succeeding — used to simulate a temporarily-down database
	// and confirm the writer retries rather than dropping the batch.
	failNextN int32
	failErr   error
	callCount int32
}

func (s *fakeSink) WriteBatch(ctx context.Context, records []Record) error {
	atomic.AddInt32(&s.callCount, 1)
	if atomic.LoadInt32(&s.failNextN) > 0 {
		atomic.AddInt32(&s.failNextN, -1)
		return s.failErr
	}
	cp := make([]Record, len(records))
	copy(cp, records)

	s.mu.Lock()
	s.batches = append(s.batches, cp)
	s.mu.Unlock()
	return nil
}

func (s *fakeSink) allRecords() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Record
	for _, b := range s.batches {
		all = append(all, b...)
	}
	return all
}

func (s *fakeSink) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *fakeSink) calls() int32 {
	return atomic.LoadInt32(&s.callCount)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() Config {
	return Config{
		QueueSize:            100,
		BatchSize:            5,
		FlushInterval:        50 * time.Millisecond,
		RetryBaseDelay:       5 * time.Millisecond,
		RetryMaxDelay:        20 * time.Millisecond,
		ShutdownFlushTimeout: time.Second,
	}
}

func rec(id string) Record {
	return Record{RequestID: id, OccurredAt: time.Now(), Endpoint: "/v1/convert", HTTPStatus: 200}
}

func TestWriter_BatchesUpToBatchSize(t *testing.T) {
	sink := &fakeSink{}
	w := New(sink, testConfig(), Hooks{}, testLogger())
	defer w.Close()

	// BatchSize is 5: enqueue exactly 5 and expect one flush without
	// waiting for FlushInterval.
	for i := 0; i < 5; i++ {
		w.Enqueue(rec("a"))
	}

	waitFor(t, func() bool { return sink.batchCount() >= 1 }, time.Second)

	if got := len(sink.allRecords()); got != 5 {
		t.Errorf("expected 5 records written, got %d", got)
	}
}

func TestWriter_FlushesOnIntervalWithPartialBatch(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.BatchSize = 100 // large enough that only the interval triggers a flush
	cfg.FlushInterval = 30 * time.Millisecond
	w := New(sink, cfg, Hooks{}, testLogger())
	defer w.Close()

	w.Enqueue(rec("a"))
	w.Enqueue(rec("b"))
	// Only 2 records, far short of BatchSize=100 — must rely on the
	// flush interval ticking to ever see these written.

	waitFor(t, func() bool { return len(sink.allRecords()) == 2 }, time.Second)
}

func TestWriter_DropsWhenQueueFull(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.QueueSize = 2
	// Make the writer never actually drain the queue during this test by
	// giving it a huge batch size and flush interval, so the 2-slot
	// buffered channel is the only thing standing between Enqueue and
	// blocking — if Enqueue ever blocked, this test would hang and fail
	// via its own deadline rather than asserting cleanly.
	cfg.BatchSize = 1_000_000
	cfg.FlushInterval = time.Hour

	var dropped int32
	w := New(sink, cfg, Hooks{
		OnDropped: func(n int) { atomic.AddInt32(&dropped, int32(n)) },
	}, testLogger())
	defer w.Close()

	// Give the background goroutine no chance to drain anything: enqueue
	// far more than QueueSize, all synchronously, right away.
	for i := 0; i < 10; i++ {
		w.Enqueue(rec("x"))
	}

	if atomic.LoadInt32(&dropped) == 0 {
		t.Error("expected at least one dropped record once the queue filled up")
	}
}

func TestWriter_EnqueueNeverBlocks(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.QueueSize = 1
	cfg.BatchSize = 1_000_000
	cfg.FlushInterval = time.Hour
	w := New(sink, cfg, Hooks{}, testLogger())
	defer w.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			w.Enqueue(rec("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked — 1000 calls against a QueueSize=1 writer should return near-instantly by dropping, never block")
	}
}

func TestWriter_RetriesFailedBatchWithBackoff(t *testing.T) {
	sink := &fakeSink{failNextN: 2, failErr: errors.New("db down")}
	cfg := testConfig()
	cfg.BatchSize = 3
	cfg.FlushInterval = time.Hour // force the batch-size trigger, not the ticker

	var errCount int32
	var written int32
	w := New(sink, cfg, Hooks{
		OnWriteError: func(err error) { atomic.AddInt32(&errCount, 1) },
		OnWritten:    func(n int) { atomic.AddInt32(&written, int32(n)) },
	}, testLogger())
	defer w.Close()

	for i := 0; i < 3; i++ {
		w.Enqueue(rec("x"))
	}

	waitFor(t, func() bool { return atomic.LoadInt32(&written) == 3 }, 2*time.Second)

	if atomic.LoadInt32(&errCount) != 2 {
		t.Errorf("expected exactly 2 recorded write errors (the 2 induced failures) before success, got %d", errCount)
	}
	if got := len(sink.allRecords()); got != 3 {
		t.Errorf("expected the batch to eventually land intact (3 records), got %d", got)
	}
}

func TestWriter_FlushesRemainingQueueOnClose(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.BatchSize = 1000          // far larger than what we enqueue
	cfg.FlushInterval = time.Hour // so only Close's drain writes anything
	w := New(sink, cfg, Hooks{}, testLogger())

	for i := 0; i < 7; i++ {
		w.Enqueue(rec("x"))
	}

	w.Close() // must flush the 7 queued-but-unbatched records before returning

	if got := len(sink.allRecords()); got != 7 {
		t.Errorf("expected all 7 queued records flushed on Close, got %d", got)
	}
}

func TestWriter_CloseTimesOutAgainstPermanentlyFailingSink(t *testing.T) {
	sink := &fakeSink{failNextN: 1 << 30, failErr: errors.New("db permanently down")}
	cfg := testConfig()
	cfg.BatchSize = 1
	cfg.FlushInterval = time.Hour
	cfg.RetryBaseDelay = 5 * time.Millisecond
	cfg.RetryMaxDelay = 10 * time.Millisecond
	cfg.ShutdownFlushTimeout = 100 * time.Millisecond

	w := New(sink, cfg, Hooks{}, testLogger())
	w.Enqueue(rec("x"))

	start := time.Now()
	w.Close()
	elapsed := time.Since(start)

	// Close must return at (or shortly after) ShutdownFlushTimeout, not
	// hang forever retrying against a sink that will never succeed.
	if elapsed > time.Second {
		t.Errorf("Close took %v against a permanently failing sink, expected it to give up near ShutdownFlushTimeout (%v)", elapsed, cfg.ShutdownFlushTimeout)
	}
}

func TestWriter_DepthReflectsQueuedRecords(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.BatchSize = 1_000_000
	cfg.FlushInterval = time.Hour
	w := New(sink, cfg, Hooks{}, testLogger())
	defer w.Close()

	for i := 0; i < 4; i++ {
		w.Enqueue(rec("x"))
	}

	if got := w.Depth(); got != 4 {
		t.Errorf("expected Depth()=4 after enqueuing 4 with no flush yet, got %d", got)
	}
}

func TestWriter_BatchWriteDurationHookFires(t *testing.T) {
	sink := &fakeSink{}
	cfg := testConfig()
	cfg.BatchSize = 1

	var sawDuration int32
	w := New(sink, cfg, Hooks{
		OnBatchWriteDone: func(d time.Duration) {
			if d >= 0 {
				atomic.StoreInt32(&sawDuration, 1)
			}
		},
	}, testLogger())
	defer w.Close()

	w.Enqueue(rec("x"))

	waitFor(t, func() bool { return atomic.LoadInt32(&sawDuration) == 1 }, time.Second)
}

// waitFor polls cond until it returns true or timeout elapses, failing
// the test on timeout. Used throughout instead of a fixed sleep, since
// the writer's flush timing is driven by goroutine scheduling, not a
// guaranteed instant.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within timeout")
	}
}
