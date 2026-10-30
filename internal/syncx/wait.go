// Package syncx provides small concurrency helpers shared across the server.
package syncx

import (
	"sync"
	"time"
)

// WaitTimeout waits for wg to reach zero and reports whether it did so before
// the timeout elapsed. A bounded wait keeps shutdown deterministic even when a
// third party engine fails to honour context cancellation.
func WaitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
