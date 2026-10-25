package memcache

import (
	"testing"
	"time"
)

// FuzzCacheModel replays byte-encoded operations against the real cache and
// the independent reference model, comparing observable results and the live
// view after every operation. Observation itself must not purge entries or
// refresh recency, so no Size/Lookup side effects are used for comparison.
func FuzzCacheModel(f *testing.F) {
	f.Add(uint8(2), []byte{0, 0, 1, 0, 1, 2, 0, 2, 3, 2, 0, 0})
	f.Add(uint8(3), []byte{1, 0, 2, 1, 0, 6, 5, 0, 3, 6, 0, 0, 2, 0, 0})
	f.Add(uint8(0), []byte{0, 0, 1, 1, 1, 2, 2, 0, 0})
	f.Add(uint8(1), []byte{1, 0, 3, 3, 0, 0, 0, 0, 7, 5, 0, 5, 7, 0, 0})
	// Writes after Close must stay rejected.
	f.Add(uint8(2), []byte{0, 1, 5, 0, 2, 6, 9, 0, 0, 0, 1, 7, 2, 1, 0})
	// TTL disabled (bit 3 set in the capacity byte) with nonpositive TTLs.
	f.Add(uint8(10), []byte{1, 0, 0, 2, 0, 0, 1, 0, 7, 2, 0, 0})
	// Consecutive advances crossing a deadline.
	f.Add(uint8(2), []byte{1, 0, 2, 5, 0, 1, 5, 0, 2, 2, 0, 0})
	f.Fuzz(func(t *testing.T, rawCapacity uint8, operations []byte) {
		if len(operations) > 2048 {
			t.Skip("bounded operation sequence")
		}
		capacity := uint(rawCapacity % 8)
		ttl := rawCapacity&8 == 0
		clock := &testClock{now: modelEpoch}
		m := newMemcache[int](capacity, ttl, clock.Now, false)
		defer m.Close()
		model := newCacheModel(int(capacity), ttl, modelEpoch)
		keys := []string{"", "a", "b", "c", "中文", "\x00", "another-key", "last"}
		for i := 0; i+2 < len(operations); i += 3 {
			key := keys[int(operations[i+1])%len(keys)]
			value := int(operations[i+2])
			var op modelOp
			switch operations[i] % 10 {
			case 0:
				op = modelOp{Kind: 0, Key: key, Value: value}
			case 1:
				op = modelOp{Kind: 1, Key: key, Value: value, TTL: int64(operations[i+2]%8) - 1}
			case 2:
				op = modelOp{Kind: 2, Key: key}
			case 3:
				op = modelOp{Kind: 3, Key: key}
			case 4:
				op = modelOp{Kind: 4}
			case 5:
				op = modelOp{Kind: 5, Advance: time.Duration(int64(operations[i+2]%5)+1) * time.Second}
			case 6:
				op = modelOp{Kind: 6}
			case 7:
				op = modelOp{Kind: 7, Key: key}
			case 8:
				op = modelOp{Kind: 8}
			case 9:
				op = modelOp{Kind: 9}
			}
			stepModelOp(t, i/3, m, clock, model, op)
		}
	})
}
