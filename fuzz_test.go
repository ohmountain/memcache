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
		m, clock, model := newModelSequenceFixture(t, capacity, ttl)
		keys := []string{"", "a", "b", "c", "中文", "\x00", "another-key", "last"}
		for i := 0; i+2 < len(operations); i += 3 {
			key := keys[int(operations[i+1])%len(keys)]
			value := int(operations[i+2])
			op := modelOp{Kind: operations[i] % 10, Key: key, Value: value}
			switch op.Kind {
			case 1:
				op.TTL = int64(operations[i+2]%8) - 1
			case 5:
				op.Advance = time.Duration(int64(operations[i+2]%5)+1) * time.Second
			}
			stepModelOp(t, i/3, m, clock, model, op)
		}
	})
}
