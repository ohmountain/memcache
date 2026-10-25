package memcache

import (
	"runtime"
)

// defaultCleanupBatchSize bounds one background reclamation batch. Stage B uses
// an internal default; publishing a Config field is Stage C work.
const defaultCleanupBatchSize = 64

// runCleaner is the one worker of a positive-capacity TTL cache. It is the
// timer's only owner and receiver, and it never holds the cache lock while
// waiting, arming, stopping, or draining the timer.
//
// Each round invalidates everything the worker previously knew: it samples the
// clock, reads the real heap root under the lock, and then either sleeps with no
// timer (empty heap), waits for the earliest deadline, or reclaims at most one
// batch. A wake carries no deadline, so a coalesced notification can never make
// the worker act on stale state.
func (s *cacheState[T]) runCleaner() {
	var timer cleanerTimer
	defer close(s.stopped)
	defer timer.stop()
	for {
		now := s.clock.Now()
		s.mu.Lock()
		closed := s.closed
		due := s.nextExpiryLocked()
		s.mu.Unlock()
		if closed {
			return
		}
		if due.IsZero() {
			// No deadline: no timer at all, and no periodic clock reads.
			timer.stop()
			select {
			case <-s.done:
				return
			case <-s.wake:
				continue
			}
		}
		if due.After(now) {
			// All timer management happens outside the cache lock. If arming
			// drained an event that had already fired, the current heap root
			// may now be due, so re-read it instead of waiting.
			if timer.arm(s.clock, due.Sub(now)) {
				continue
			}
			select {
			case <-s.done:
				return
			case <-s.wake:
				continue
			case <-timer.channel():
				timer.consumed()
				continue
			}
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		removed := s.purgeExpiredBatchLocked(now, defaultCleanupBatchSize)
		s.mu.Unlock()
		if removed == 0 {
			// The root changed between the read and the lock; re-read it.
			continue
		}
		// Between batches, prefer shutdown and let foreground writers run.
		select {
		case <-s.done:
			return
		default:
		}
		runtime.Gosched()
	}
}
