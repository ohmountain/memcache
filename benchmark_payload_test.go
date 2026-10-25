package memcache

import (
	"strconv"
	"testing"
)

// BenchmarkCachePayload compares hit lookups and overwrite updates for three
// value shapes at fixed capacity 1024: scalar int, a 256-byte array copied
// by value on every store, and a single pre-allocated 4096-byte slice shared
// by every key so stores copy only the slice header. Payload allocation
// happens in the preparation phase and never enters the timed loop.
func BenchmarkCachePayload(b *testing.B) {
	b.Run("int", func(b *testing.B) {
		keys := payloadKeys()
		values := make([]int, len(keys))
		for i := range values {
			values[i] = i
		}
		benchmarkPayloadOps(b, keys, values, "scalar int")
	})
	b.Run("bytes256", func(b *testing.B) {
		keys := payloadKeys()
		values := make([][256]byte, len(keys))
		for i := range values {
			values[i] = [256]byte{byte(i)}
		}
		benchmarkPayloadOps(b, keys, values, "256-byte array copied by value")
	})
	b.Run("bytes4k", func(b *testing.B) {
		keys := payloadKeys()
		payload := make([]byte, 4096)
		values := make([][]byte, len(keys))
		for i := range values {
			values[i] = payload
		}
		benchmarkPayloadOps(b, keys, values, "one pre-allocated 4096-byte slice shared by all keys")
	})
}

func payloadKeys() []string {
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

func benchmarkPayloadOps[T CacheAble](b *testing.B, keys []string, values []T, holding string) {
	m := WithLRU[T](1024, false)
	b.Cleanup(m.Close)
	for i, key := range keys {
		m.Set(key, values[i])
	}
	b.Logf("payload holding: %s", holding)
	b.Run("Lookup", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		hits := 0
		for i := 0; i < b.N; i++ {
			if _, ok := m.Lookup(keys[i%len(keys)]); ok {
				hits++
			}
		}
		b.StopTimer()
		if hits != b.N {
			b.Fatalf("hit fixture missed %d of %d", b.N-hits, b.N)
		}
	})
	b.Run("Set", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.Set(keys[i%len(keys)], values[i%len(values)])
		}
		b.StopTimer()
	})
}
