# Changelog

## Unreleased

* `GetCopy` and `View` no longer allocate on LZ4 files (the decoded block lives in a pooled scratch buffer; the value is copied into the caller's buffer or passed to the callback). `Get` still allocates one copy per lookup on LZ4 files because its result outlives the call: use `GetCopy` or `View` when you run LZ4. LZ4 lookups decode a whole block, so they stay slower than plain files (about 2x P50 on the 15M-key benchmark); the sub-microsecond P99 figure applies to plain v2 files only.

## 1.4.0 - 2026-10-31

* Rename dataset to stash in the public API: `Synchronizer.StashName()` (`DatasetName()` is deprecated) and the `stashID` parameter of `NewSynchronizer`. The wire protocol is unchanged.
* Fail closed with an explicit error when a file uses a format feature newer than the SDK: `*FeatureError` (`errors.Is(err, ErrUnsupportedFeature)`) for unknown flag bits, offset widths and fingerprint widths on an intact file; `VersionError` now says to upgrade.
* `Map.GetMany`: batch lookup that pins the mapping once and issues a group of keys' table reads together (zero allocations; about 6-10 % less time per key than `Get` on the 15M-key benchmark file with random keys, a quiet-host figure is still to be measured).
* `Schema`: optional typed values (fixed layout shared with the Rust and Java SDKs; no format change).
* Metrics: `Map.Misses`, `Map.LatencyBuckets`, `Map.WritePrometheus`, `Synchronizer.WritePrometheus` and `MetricsHandler` (hit ratio, lookup latency histogram, file version, sync and CRC errors).
* Signed artifacts: `WithSignatureKeys` enforces Ed25519 signatures (`X-Attos-Signature`, detached, file format unchanged) on downloaded and cached files; fails closed (`ErrSignatureMissing`, `ErrSignatureInvalid`) and keeps serving the verified file.
* `cmd/attos-size`: samples a CSV, a key,value export or a live Redis and prints projected Attos file sizes, RAM saved and equivalent Redis cost.

## v1.3.0 (2026-10-22)

* PTHash v2 format with a minimal perfect hash and optional LZ4 block compression.
* Adaptive offset width: 40-bit offsets lift the 4 GiB blob limit.
* Spilling `Builder` for low-memory builds.
* Lower serving RSS: no default warm-up, `MADV_RANDOM`, pages released after the CRC pass. `OpenConfig.Warm` modes (none, index, full).
* Opt-in lean keys: fingerprints instead of stored keys.

## v1.2.0

* Conditional sync (ETag/304), validate-then-swap hot swap, 402/429 back-off, API-key telemetry. Reads format v1 only.
