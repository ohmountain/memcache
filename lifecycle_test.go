package memcache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
		select {
		case <-m.state.stopped:
		default:
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
			select {
			case <-state.done:
				select {
				case <-state.stopped:
					return Lives() == before
				default:
				}
			default:
			}
			return false
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

func abandonHandle(expiry bool) *cacheState[int] {
	m := WithLRU[int](2, expiry)
	m.Set("k", 1)
	return m.state
}

func TestCleanerRuns(t *testing.T) {
	m := testCache(t, 1, true)
	m.SetExpire("k", 1, 1)
	waitUntil(t, "background expiration", func() bool {
		m.state.mu.Lock()
		defer m.state.mu.Unlock()
		return len(m.state.holder) == 0
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
