package memcache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCloseUnregistersAndRejectsWrites(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		before := Lives()
		m := WithLRU[int](2, expiry)
		m.SetExpire("ttl", 1, 10)
		m.Set("plain", 2)
		if Lives() != before+1 {
			t.Fatal("new cache not registered")
		}
		m.Close()
		m.Close()
		if Lives() != before {
			t.Fatal("closed cache still registered")
		}
		if cleanerRunning(m) {
			t.Fatal("Close returned before cleaner stopped")
		}
		m.Set("a", 1)
		m.SetExpire("b", 2, 1)
		m.Delete("a")
		m.Clear()
		if m.Size() != 0 || m.Cap() != 2 {
			t.Fatal("closed cache changed size/capacity")
		}
		requireMissing(t, m, "plain")
		requireMissing(t, m, "a")
		assertInvariants(t, m)
	}
}

func TestMonitorClearKeepsCachesUsable(t *testing.T) {
	before := Lives()
	a := testCache(t, 2, false)
	b := testCache(t, 2, true)
	a.Set("a", 1)
	b.SetExpire("b", 2, 60)
	CLEAR_ALL()
	if Lives() != before+2 || a.Size() != 0 || b.Size() != 0 {
		t.Fatal("global clear changed registry or retained data")
	}
	a.Set("a", 3)
	b.SetExpire("b", 4, 60)
	requireValue(t, a, "a", 3)
	requireValue(t, b, "b", 4)
}

func TestAbandonedHandleCanBeCollected(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		before := Lives()
		state := abandonHandle(expiry)
		t.Cleanup(state.close)
		waitUntil(t, "abandoned handle collection", func() bool {
			runtime.GC()
			// done is nil when no worker was ever started (the cache never
			// held a TTL), in which case unregistration alone proves the
			// finalizer ran.
			if state.done != nil {
				select {
				case <-state.done:
				default:
					return false
				}
				select {
				case <-state.stopped:
				default:
					return false
				}
			}
			return Lives() == before
		})
	}
}

func waitUntil(t *testing.T, description string, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

// TestConcurrentCloseWaitsForTeardown pins that a Close which observes a
// cache already being closed waits for the first Close to finish emptying and
// unregistering it, instead of returning as soon as the cleaner exits.
//
// The window is one shard clear per Close and whether a given closer lands in
// it depends on scheduling, so this is a probabilistic detector: at 50
// iterations it missed the pre-fix bug in 7 of 40 runs across two batches. 400
// iterations cost about 0.3s and caught it in 10 of 10, so the count stays
// large on purpose — shrinking it makes the test quietly stop guarding
// anything.
//
// Stats is the observable channel: Len and Size both short-circuit to 0 once
// closed, so asserting on either would make this vacuous.
func TestConcurrentCloseWaitsForTeardown(t *testing.T) {
	const (
		iterations = 400
		closers    = 4
		entries    = 4096
	)
	for iteration := 0; iteration < iterations; iteration++ {
		before := Lives()
		m := WithOptions[int](Options{Capacity: entries, Shards: 64, EnableExpired: true})
		for i := 0; i < entries; i++ {
			m.SetExpireDuration(workloadKey(i), i, time.Hour)
		}

		var wg sync.WaitGroup
		failures := make(chan string, closers)
		wg.Add(closers)
		for i := 0; i < closers; i++ {
			go func() {
				defer wg.Done()
				m.Close()
				if got := m.Stats().Entries; got != 0 {
					failures <- fmt.Sprintf("Close returned with %d entries still present", got)
					return
				}
				if got := Lives(); got != before {
					failures <- fmt.Sprintf("Close returned while still registered: Lives = %d, want %d", got, before)
				}
			}()
		}
		wg.Wait()
		close(failures)
		for msg := range failures {
			t.Fatalf("iteration %d: %s", iteration, msg)
		}
	}
}

func abandonHandle(expiry bool) *cacheState[int] {
	m := WithLRU[int](2, expiry)
	if expiry {
		// Start the cleaner so the finalizer is also exercised on the
		// join-the-worker path, not only on the no-worker path.
		m.SetExpire("k", 1, 3600)
	} else {
		m.Set("k", 1)
	}
	return m.state
}

func TestCleanerRuns(t *testing.T) {
	m := testCache(t, 1, true)
	m.SetExpire("k", 1, 1)
	waitUntil(t, "background expiration", func() bool {
		return m.state.Len() == 0
	})
}

func TestCacheDoesNotInterceptInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("requires Unix process signals")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalHelper$")
	cmd.Env = append(os.Environ(), "MEMCACHE_SIGNAL_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "READY\n" {
		t.Fatalf("helper readiness = %q, %v", ready, err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || ctx.Err() != nil {
		t.Fatalf("SIGINT did not terminate helper: wait=%v context=%v", err, ctx.Err())
	}
}

func TestSignalHelper(t *testing.T) {
	if os.Getenv("MEMCACHE_SIGNAL_HELPER") != "1" {
		return
	}
	m := WithLRU[int](1, true)
	defer m.Close()
	fmt.Println("READY")
	time.Sleep(30 * time.Second)
}
