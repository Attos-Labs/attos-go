package hash

// String implements a deterministic 64-bit FNV-1a hash.
// Identical to the backend compile.hashKey — they MUST match.
func String(key string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

// Bytes implements a deterministic 64-bit FNV-1a hash for byte slices.
func Bytes(key []byte) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

// Block hashes a block using compression functions.
func Block(key, salt uint64, lvl uint32) uint64 {
	const m uint64 = 0x880355f21e6d1965
	h := m
	h ^= mix(key)
	h *= m
	h ^= mix(salt)
	h *= m
	h ^= mix(uint64(lvl))
	h *= m
	h = mix(h)
	return h
}

// mix is a compression function for hash.
func mix(h uint64) uint64 {
	h ^= h >> 23
	h *= 0x2127599bf4325c37
	h ^= h >> 47
	return h
}
