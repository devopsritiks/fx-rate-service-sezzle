package resilience

import "context"

// bulkhead limits how many calls to the vendor can be in flight at once,
// independent of how many inbound HTTP requests we're handling. The name
// comes from ship design: a bulkhead is a partition that stops one
// flooded compartment from sinking the whole vessel. Here, the
// compartment is "goroutines/connections talking to Frankfurter" — if the
// vendor goes slow, without a cap every inbound request would pile up a
// blocked goroutine and a held connection waiting on it, until we run out
// of one or the other. The bulkhead caps that blast radius to a fixed
// number, so a slow vendor degrades us (callers past the cap wait or get
// a clear error) instead of taking down the whole process.
type bulkhead struct {
	sem chan struct{}
}

func newBulkhead(maxConcurrent int) *bulkhead {
	return &bulkhead{sem: make(chan struct{}, maxConcurrent)}
}

// acquire blocks until a slot is free or ctx is done. Returning promptly
// on ctx cancellation means a client that has already disconnected (or
// whose overall budget expired) doesn't sit queued for a bulkhead slot
// it'll never get to use.
func (b *bulkhead) acquire(ctx context.Context) error {
	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *bulkhead) release() {
	<-b.sem
}
