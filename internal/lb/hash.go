package lb

// Hash64 hashes s with 64-bit FNV-1a followed by murmur3's fmix64 finaliser.
//
// FNV-1a alone mixes its last bytes poorly, so strings that differ only at
// the end, such as the virtual-node names "addr#1", "addr#2", land close
// together on the ring. The finaliser spreads every input bit over the whole
// word. The loop is written out because hash/fnv returns a hash.Hash64
// interface, which allocates. The result is the same in every process, so
// every proxy instance builds the same ring from the same config.
func Hash64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
