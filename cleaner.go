package memcache

import (
	"runtime"
	"time"
)

// defaultCleanupBatchSize bounds one background reclamation batch. The worker
// raises it for very large expiry heaps so a million-entry sweep is not split
// into tens of thousands of lock round-trips.
const defaultCleanupBatchSize = 64

// runCleaner is the one worker of a positive-capacity TTL cache. It is the
// timer's only owner and receiver, and it never holds a shard lock while
// waiting, arming, stopping, or draining the timer.
//
// Each round invalidates everything the worker previously knew: it samples the
// clock, reads the earliest deadline across shards, and then either sleeps with
// no timer (empty heaps), waits for that deadline, or reclaims one bounded
// batch. A wake carries no deadline, so a coalesced notification can never make
// the worker act on stale state.
func (s *cacheState[T]) runCleaner() {
	var timer cleanerTimer
	defer s.closeStopped()
	defer timer.stop()
	for {
		now := s.nowNanos()
		if s.isClosed() {
			return
		}
		due, expiryRecords := s.deadlineSnapshot()
		if s.isClosed() {
			// Close may have landed while the snapshot held shard locks.
			return
		}
		if due == 0 {
			// No deadline: no timer at all, and no periodic clock reads.
			timer.stop()
			select {
			case <-s.done:
				return
			case <-s.wake:
				continue
			}
		}
		if due > now {
			// All timer management happens outside every shard lock. If arming
			// drained an event that had already fired, the current roots may now
			// be due, so re-read them instead of waiting.
			if timer.arm(s.clock, time.Duration(due-now)) {
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
		removed := s.purgeDueBatch(now, cleanupBatchSize(expiryRecords))
		if removed == 0 {
			// A root changed between the read and the lock; re-read it.
			continue
		}
		select {
		case <-s.done:
			return
		default:
		}
		// Yield between batches so foreground writers are not starved.
		runtime.Gosched()
	}
}
