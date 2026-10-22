# .attos format, version 2 (PTHash)

Version 1 files (hash-and-displace) remain readable. Version 2 is what `Builder.Compile` writes.
Header fields are big-endian (as in v1); the bulk arrays are little-endian so they can be read natively.

All hashing starts from `x = XXH64(keyBytes, seed 0)` (unchanged from v1).

## Layout

```
offset  size  field
0       4     magic "ATTO"
4       4     version = 2 (u32 BE)
8       1     flags: bit 0 = records are stored in compressed blocks (see below); bit 1 = lean keys (see below); all other bits must be 0
9       1     frontWidth  (bits per pilot of the front buckets, 1..32)
10      1     backWidth   (bits per pilot of the back buckets, 1..32)
11      1     blockShift (only when flags bit 0 is set: records per block = 1 << blockShift, 0..8); otherwise 0
12      8     numKeys   n  (u64 BE)
20      8     tableSize m  (u64 BE) = ceil(n / 0.99), n <= m <= 2n+16
28      8     numBuckets nb (u64 BE)
36      8     numFront  p2 (u64 BE)  buckets [0,p2) are "front", [p2,nb) are "back"
44      8     p1thr        (u64 BE)  skew threshold
52      1     offsetWidth: bytes per entry of the offsets section. 0 or 4 = u32 LE, 5 = 40-bit LE; any other value is corrupt
53      1     fingerprint width: 4 or 8 when flags bit 1 is set, else 0 (ignored)
54      10    reserved = 0
64      ...   sections, in this order, no gaps:
              front pilots   PackedSize(p2, frontWidth)
              back pilots    PackedSize(nb-p2, backWidth)
              free slots     FreeSize(n, m)
              offsets        offsetsSize(nOffsets, offsetWidth); nOffsets = n+1, or ceil(n / (1<<blockShift)) + 1 when compressed
              blob           offsets[n] bytes
              crc32          4 bytes, u32 BE, CRC32-IEEE of every preceding byte
```

`PackedSize(count, w) = ceil(count*w / 64) * 8 + 8`  (bit-packed, + 8 bytes of padding so an unaligned 8-byte read at any element is in bounds)
`FreeSize(n, m) = ((m-n)*4 rounded up to a multiple of 8)`

* Pilot i of width w sits at bit offset `i*w` of its section, least-significant bit first
  (read: `(LE64(section[bit>>3 ..]) >> (bit&7)) & (2^w - 1)`).
* free slots: `m-n` little-endian u32 values.
* `offsetsSize(count, w) = ceil(count*w / 8) * 8`, plus 8 bytes of slack when w = 5 (so an unaligned u64 load at any element is in bounds; mask `0xFFFFFFFFFF`). Writers choose w = 4 when the blob is at most 2^32-1 bytes, else 5; blobs above 2^40-1 bytes are rejected. Width 4 files are byte-identical to files written before this field existed.
* offsets: `n+1` little-endian values of `offsetWidth` bytes; record of slot s is `blob[offsets[s] : offsets[s+1]]`.
* record = `u16 BE keyLength | key bytes | value bytes` (same as v1). Readers MUST compare the stored key
  with the lookup key; the hash function maps unknown keys to arbitrary slots.

## Lookup (x = XXH64(key))

```
r = rotl64(x, 32)
if x <  p1thr:  p = pilotFront[ reduce(r, p2) ]
else:           p = pilotBack [ reduce(r, nb - p2) ]
q = reduce(mix(x ^ hp(p)), m)
if q >= n:  q = LE32(freeSlots[(q - n)*4 ...])
slot = q                      // 0 <= slot < n, a bijection on the build keys
```

```
mix(x):  x ^= x>>32; x *= 0xD6E8FEB86659FD93; x ^= x>>32; x *= 0xD6E8FEB86659FD93; x ^= x>>32   (all u64, wrapping, logical shifts)
hp(p):   mix(p + 0x1234567)
reduce(h, n) = high 64 bits of the unsigned 128-bit product h*n
```

Golden checks: `mix(1)=0x4179b061e0c0e0d0`, `mix(0xDEADBEEF)=0x288344a3928b2668`, `hp(0)=0xc5209269cd34c3fc`, `reduce(2^63, 10)=5`.

## Compressed blocks (flags bit 0)

The key lookup (`slot`) is unchanged. The `offsets` table then indexes **blocks** of `B = 1 << blockShift`
consecutive slots: block `b` holds slots `b*B .. min((b+1)*B, n) - 1` and occupies `blob[offsets[b] : offsets[b+1]]`.

```
block   = method (1 byte) | uvarint(rawLen) | payload
method 0: payload is the raw content (len(payload) == rawLen)
method 1: payload is an LZ4 *block* (no frame header, https://github.com/lz4/lz4/blob/dev/doc/lz4_Block_format.md)
          that decompresses to exactly rawLen bytes
raw     = for each slot in the block, in slot order:  uvarint(recordLen) | record
record  = u16 BE keyLength | key | value        (identical to the uncompressed record)
```

* To read slot `q`: take block `q >> blockShift`, decode it, skip `q & (B-1)` records, read the next.
* uvarint = unsigned LEB128 as in Go's `binary.Uvarint` / protobuf.
* Readers must bound `rawLen` (reject > 2^28, and for method 1 reject `rawLen > 255*len(payload) + 16`), reject
  `offsets[b+1] <= offsets[b]`, and never read outside the file. The LZ4 decoder must be bounds-checked; matches may
  overlap their own output (offset < match length); offset 0 is invalid.
* Latency cost: a lookup decodes one block (about 0.4 us per 240-byte record in the block on the test VM, so about 3 us at 8 records per block, on top of the uncompressed lookup),
  and returns a copy of the value rather than a view of the mapping.

## Reader validation (must not panic or read out of bounds on hostile input)

n >= 1, n <= 2^32-1, n <= m <= 2n+16, nb >= 2, 1 <= p2 < nb, widths in 1..32, the sections must fit in the file
(with 4 bytes of CRC), and `offsets[n]` plus the blob start must fit in the file. Per lookup, the record
`[start,end)` must satisfy `start <= end <= blobLength`; otherwise report corruption.

## Test vectors

`testdata/golden_v2_{1,1000,5000}.attos` with `.tsv` files (one line per row: hex(key) TAB hex(value)).
Compressed variants (same rows as the matching .tsv): `golden_v2_lz4b16_1.attos` (uses golden_v2_1.tsv), `golden_v2_lz4b1_1000`, `golden_v2_lz4b16_1000`, `golden_v2_lz4b256_1000` (golden_v2_1000.tsv), `golden_v2_lz4b16_5000` (golden_v2_5000.tsv).
They include empty values, NUL bytes inside keys, non-ASCII keys and long keys/values.

## Lean keys (flags bit 1)

Opt-in. Records store a fixed-size fingerprint of the key instead of the key, saving `keyLength` bytes per
record at the price of a tiny false-accept rate for keys that were never added.

```
record  = fingerprint (fpWidth bytes, 4 or 8, from header byte 53) | value          (no u16 key length)
```

* `fingerprint(key)`: `h = 0xcbf29ce484222325; for each byte c: h = (h ^ c) * 0x100000001b3` (FNV-1a 64), then
  `h ^= h>>32; h *= 0xD6E8FEB86659FD93; h ^= h>>32; h *= 0xD6E8FEB86659FD93; h ^= h>>32` (the PTHash mix),
  and the fingerprint is the first `fpWidth` bytes of the **big-endian** encoding of `h`. It is independent of the
  XXH64 hash PTHash uses.
* Lookup: resolve the slot as usual, read the record (plain or from its LZ4 block), compare the first `fpWidth`
  bytes with `fingerprint(key)`; equal means found, the value is the rest of the record. A key that was never
  added is accepted with probability 2^-32 (width 4) or 2^-64 (width 8) and would return another key's value.
* Scan yields the fingerprint in place of the key.
* Builders still reject duplicate keys using the full keys before writing. Lean works with LZ4 blocks (the raw
  block content is `uvarint(recordLen) | record` as before) and with offset width 5.
* Readers must reject `fpWidth` other than 4 or 8 when bit 1 is set, and treat a record shorter than `fpWidth` as corrupt.
