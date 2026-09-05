// Package closegate provides the bounded admission wait shared by ACP client
// teardown paths.
package closegate

import (
	"context"
	"sync"
	"time"
)

// PromptDrainGrace is the existing maximum protocol-write window. Close lets
// an ordinary admitted Prompt finish for at most this long so session/close
// retains wire order; a backpressured Prompt is preempted after the grace.
const PromptDrainGrace = 250 * time.Millisecond

// Once serializes an idempotent close operation and publishes its result only
// after the owning cleanup has finished. sync.Once deliberately makes
// concurrent followers wait for the owner instead of observing an early
// "closed" transition while transport cleanup is still in flight.
type Once struct {
	once sync.Once
	err  error
}

// Do performs closeFn at most once and returns the owning invocation's result.
func (o *Once) Do(closeFn func() error) error {
	o.once.Do(func() {
		o.err = closeFn()
	})
	return o.err
}

// TryLockWithin acquires mu before ctx or maxWait expires. It never leaves a
// background waiter behind: timed polling is required because a goroutine
// blocked in Mutex.Lock would eventually acquire ownership after Close had
// already returned, with no safe owner left to unlock it.
func TryLockWithin(ctx context.Context, mu *sync.Mutex, maxWait time.Duration) bool {
	if mu == nil {
		return false
	}
	if maxWait <= 0 {
		return mu.TryLock()
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if mu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}
