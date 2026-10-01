package audit

import (
	"context"
	"log/slog"
	"math/rand"
	"time"
)

// Sink is the thing a Writer flushes batches to. Defined here
// (consumer side) so the batch-writing/queueing logic can be tested
// with a fake in-memory sink and no real Postgres — see writer_test.go.
// The real implementation (pgSink, in pg_sink.go) wraps a pgxpool.Pool.
type Sink interface {
	// WriteBatch persists records. It must be safe to call concurrently
	// with itself only in the sense that Writer never does — Writer calls
	// WriteBatch from a single goroutine, one call at a time, so Sink
	// implementations don't need their own internal locking for this.
	WriteBatch(ctx context.Context, records []Record) error
}

// Hooks lets callers observe writer activity for a metrics phase without
// this package depending on Prometheus directly — same pattern as every
// other Hooks struct in this codebase (internal/cache, internal/ratelimit,
// internal/fxvendor/*).
type Hooks struct {
	OnWritten        func(n int)
	OnDropped        func(n int)
	OnWriteError     func(err error)
	OnBatchWriteDone func(duration time.Duration)
	// OnQueueDepth is sampled periodically (see Writer.Depth, called from
	// main.go's gauge-sampling ticker) rather than pushed on every
	// enqueue/dequeue — queue depth is a point-in-time fact, not an event,
	// same reasoning as the cache-entries gauge in internal/cache.
}

// Config controls batching and backpressure behavior.
type Config struct {
	// QueueSize bounds how many records can be buffered waiting to be
	// written. Once full, Enqueue drops the new record immediately rather
	// than blocking the caller — see Writer's doc comment for why.
	QueueSize int
	// BatchSize is the max number of records written in one INSERT.
	BatchSize int
	// FlushInterval is the max time a partial batch waits before being
	// written anyway, so low-traffic periods don't hold records
	// indefinitely waiting for a batch to fill.
	FlushInterval time.Duration
	// RetryBaseDelay / RetryMaxDelay control full-jitter backoff between
	// failed WriteBatch attempts — same backoff shape as
	// internal/fxvendor/resilience uses against the vendor, for the same
	// thundering-herd-avoidance reason.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	// ShutdownFlushTimeout bounds how long Close will wait to drain the
	// in-memory queue to the sink before giving up.
	ShutdownFlushTimeout time.Duration
}

// Writer is an async, best-effort audit log writer.
//
// The design, and the tradeoff it deliberately makes:
//
// Enqueue is called from the HTTP hot path and must never slow down or
// fail a request because of audit logging. It does a non-blocking send
// into a fixed-size buffered channel: if the channel has room, the
// record is queued and Enqueue returns immediately; if the channel is
// full, the record is DROPPED on the spot and a counter is incremented.
// Enqueue never blocks, never errors, and is safe to call from every
// request regardless of what Postgres is doing.
//
// A single background goroutine (run) owns the channel's receive side.
// It accumulates records into a batch and flushes (one WriteBatch call)
// whenever either BatchSize records have accumulated or FlushInterval has
// elapsed since the last flush, whichever comes first — so a quiet period
// doesn't leave a handful of records sitting unwritten indefinitely. If a
// flush fails (e.g. Postgres is down), it retries that same batch with
// full-jitter backoff rather than dropping it outright or blocking
// newly-queued records from accumulating in the meantime — new records
// keep queueing (and can still be dropped on overflow) while a failed
// batch retries in the background.
//
// On Close, whatever is still in the channel is drained and flushed
// (ignoring BatchSize, writing everything in one or more final batches)
// within ShutdownFlushTimeout; past that deadline, remaining records are
// dropped and counted rather than hanging process shutdown indefinitely.
//
// Why dropping is an accepted tradeoff here, and what a real fintech
// audit log would do instead: dropping records under backpressure is
// almost certainly NOT acceptable for a regulated financial audit trail
// — SOX/PCI-adjacent compliance regimes generally require a complete,
// durable audit record, not a best-effort one. In production I would not
// ship "drop and count" as the final answer; I'd use a transactional
// outbox (write the audit event durably as part of the same operation,
// via an outbox table or a durable local log, then have a separate
// process reliably deliver it) or publish audit events to Kafka/Kinesis
// with at-least-once delivery, where the API's hot path only has to
// durably enqueue to a replicated log (fast, and itself highly
// available) rather than wait on Postgres directly — Postgres being down
// then stalls a downstream consumer's writes, not the API, and the
// durable log's own retention covers the outage window instead of a
// fixed-size in-memory channel. What's built here has the right *shape*
// (never block the request path) but a deliberately weaker durability
// guarantee, appropriate for this project's scope, not for a production
// regulated audit trail as-is.
type Writer struct {
	sink   Sink
	cfg    Config
	hooks  Hooks
	logger *slog.Logger

	queue chan Record
	done  chan struct{}

	// runCtx bounds every write (including retries) the background
	// goroutine performs. Close cancels it once ShutdownFlushTimeout
	// elapses, which both aborts an in-progress retry-backoff sleep and
	// makes the next WriteBatch call fail fast via ctx — without this, a
	// Writer shutting down against a permanently-dead sink would retry
	// forever in the background after Close already gave up waiting,
	// leaking the goroutine for the life of the process.
	runCtx    context.Context
	runCancel context.CancelFunc
}

// New creates a Writer and starts its background batch-writing goroutine.
// Call Close to flush and stop it.
func New(sink Sink, cfg Config, hooks Hooks, logger *slog.Logger) *Writer {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{
		sink:      sink,
		cfg:       cfg,
		hooks:     hooks,
		logger:    logger,
		queue:     make(chan Record, cfg.QueueSize),
		done:      make(chan struct{}),
		runCtx:    ctx,
		runCancel: cancel,
	}
	go w.run()
	return w
}

// Enqueue queues rec for writing. It never blocks: if the queue is full,
// rec is dropped immediately and OnDropped(1) fires. This is the entire
// point of this package — see the Writer doc comment.
func (w *Writer) Enqueue(rec Record) {
	select {
	case w.queue <- rec:
	default:
		if w.hooks.OnDropped != nil {
			w.hooks.OnDropped(1)
		}
		if w.logger != nil {
			w.logger.Warn("audit_queue_full_dropping_record", "request_id", rec.RequestID)
		}
	}
}

// Depth reports the current number of queued-but-not-yet-written
// records, for a periodically-sampled gauge (see main.go).
func (w *Writer) Depth() int {
	return len(w.queue)
}

// run is the single background goroutine that owns the queue's receive
// side, batches records, and flushes them to the sink.
func (w *Writer) run() {
	defer w.runCancel()
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, w.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.writeWithRetry(w.runCtx, batch)
		batch = batch[:0]
	}

	for {
		select {
		case rec, ok := <-w.queue:
			if !ok {
				// w.queue is closed AND fully drained at this point — a
				// receive on a closed channel only ever reports ok=false
				// once every buffered value has already been delivered
				// through the ok=true case above, so there is nothing left
				// to read here. Flush whatever's left in the in-progress
				// batch and we're done.
				flush()
				close(w.done)
				return
			}
			batch = append(batch, rec)
			if len(batch) >= w.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// writeWithRetry writes batch to the sink, retrying with full-jitter
// backoff on failure until ctx is done. A copy of batch is taken before
// the first attempt so the caller's slice (reused via batch[:0]) can't be
// mutated out from under a retry.
func (w *Writer) writeWithRetry(ctx context.Context, batch []Record) {
	records := make([]Record, len(batch))
	copy(records, batch)

	attempt := 0
	for {
		start := time.Now()
		err := w.sink.WriteBatch(ctx, records)
		duration := time.Since(start)

		if w.hooks.OnBatchWriteDone != nil {
			w.hooks.OnBatchWriteDone(duration)
		}

		if err == nil {
			if w.hooks.OnWritten != nil {
				w.hooks.OnWritten(len(records))
			}
			return
		}

		if w.hooks.OnWriteError != nil {
			w.hooks.OnWriteError(err)
		}
		if w.logger != nil {
			w.logger.Error("audit_batch_write_failed", "error", err.Error(), "batch_size", len(records), "attempt", attempt+1)
		}

		delay := backoffDelay(attempt, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay)
		attempt++

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// backoffDelay is the same full-jitter exponential backoff shape used in
// internal/fxvendor/resilience, duplicated rather than shared across
// packages to avoid a cross-dependency between audit logging and vendor
// resilience for what is a ~5-line, well-understood algorithm.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	cap := time.Duration(1<<uint(attempt)) * base
	if cap > max || cap <= 0 {
		cap = max
	}
	return time.Duration(rand.Int63n(int64(cap) + 1))
}

// Close stops accepting new records' background processing, flushes
// whatever is queued within ShutdownFlushTimeout, and returns once done
// (or once the timeout elapses, whichever is first). Safe to call once;
// Enqueue after Close will panic (sending on a closed channel), which is
// acceptable since main.go only calls Close during final shutdown, after
// the HTTP server itself has already stopped accepting requests.
func (w *Writer) Close() {
	close(w.queue)
	select {
	case <-w.done:
	case <-time.After(w.cfg.ShutdownFlushTimeout):
		if w.logger != nil {
			w.logger.Warn("audit_shutdown_flush_timed_out")
		}
		// Abort whatever the background goroutine is doing (an
		// in-progress write or a retry backoff sleep) so it actually
		// exits instead of continuing to retry against a sink that may
		// be permanently unavailable, leaking the goroutine for the rest
		// of the process's life.
		w.runCancel()
		<-w.done
	}
}
