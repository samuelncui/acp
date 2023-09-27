package acp

func Cache[K comparable, V any](f func(in K) V) func(in K) V {
	cache := make(map[K]V, 0)
	return func(in K) V {
		cached, has := cache[in]
		if has {
			return cached
		}

		out := f(in)
		cache[in] = out
		return out
	}
}
