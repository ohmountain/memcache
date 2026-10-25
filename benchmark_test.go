package memcache

import (
	"strconv"
	"testing"
)

func BenchmarkCache(b *testing.B) {
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	for _, operation := range []string{"Lookup", "Get", "Set", "RenewTTL"} {
		b.Run(operation, func(b *testing.B) {
			m := WithLRU[int](1024, operation == "RenewTTL")
			defer m.Close()
			for i, key := range keys {
				m.Set(key, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := keys[i%len(keys)]
				switch operation {
				case "Lookup":
					m.Lookup(key)
				case "Get":
					m.Get(key)
				case "Set":
					m.Set(key, i)
				case "RenewTTL":
					m.SetExpire(key, i, 60)
				}
			}
			b.StopTimer()
		})
	}
	b.Run("ParallelLookup", func(b *testing.B) {
		m := WithLRU[int](1024, false)
		defer m.Close()
		for i, key := range keys {
			m.Set(key, i)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				m.Lookup(keys[i%len(keys)])
				i++
			}
		})
		b.StopTimer()
	})
}
