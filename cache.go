package acp

import "sync"

type cacheEntry[V any] struct {
	once  sync.Once
	value V
}

func Cache[K comparable, V any](f func(in K) V) func(in K) V {
	var cache sync.Map
	return func(in K) V {
		cached, has := cache.Load(in)
		if !has {
			cached, _ = cache.LoadOrStore(in, new(cacheEntry[V]))
		}

		entry := cached.(*cacheEntry[V])
		entry.once.Do(func() { entry.value = f(in) })
		return entry.value
	}
}
