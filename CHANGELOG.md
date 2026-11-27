# Changelog

## Unreleased

* Patch updates simplified: the copy/add patch (ATDL v1) is gone; the overflow tail patch (ATDL v2) is the only patch type. It is negotiated by `X-Attos-Capabilities: patch` (replacing `overflow` and `X-Attos-Accept-Delta`, which are no longer sent), and it applies to any file that starts with the chain's main blob, so a node several versions behind takes the same one patch as a node one behind. The patch header's base size and CRC are no longer checked; the target SHA-256 proves the rebuild. `WithDelta(false)` still turns it off.
* The redirect cooldown is fixed at one hour; the exported `RedirectCooldown` variable is removed.
* Overflow records are validated when a file is opened (not at the first lookup that meets one), and a v2 (tail) patch refuses the v1 copy/add ops: both match the other four SDKs and the spec (OVERFLOW_BLOCK.md, OVERFLOW_SHIPPED.md). A file that opened is guaranteed to hold only well-formed records.

* Rename stash back to dataset in the public API: `Synchronizer.DatasetName()` is the name again and `StashName()` (added in 1.4.0) stays as a deprecated alias that returns the same value; the `NewSynchronizer` parameter is called `datasetID` (positional, so no source change). Prometheus metrics and docs say dataset. The wire protocol is unchanged.
* Warm-up by default in the Synchronizer: every newly mapped file is read into memory before it is published (`OpenConfig.WarmUp`), so a hot swap never exposes readers to a cold mapping (first-touch page faults added about 350 to 600 ns to P99 at 100k reads/s). Opt out with `WithWarmUp(false)`; an explicit `OpenConfig.Warm`/`WarmUp` is always honoured. Cost: resident memory equals the file size.

## 1.5.0 - 2026-11-09

* Delta updates (on by default; `WithDelta(false)` turns them off): a node that is one version behind downloads a small binary patch instead of the whole blob, rebuilds the new blob into a new file and runs the same validate-then-swap as a full download (CRC, signature, atomic publish). Any failure falls back to the full blob inside the same `Sync`. `Synchronizer.DeltaStats` reports patches applied, fallbacks and patch bytes. Servers that do not know about patches keep sending full blobs.
* Overflow block: reads v2 files that carry a small sorted side table between the blob and the CRC (v2 flags bit 2) for inserted, changed and deleted keys, so an update can ship as a tail patch of the changed keys instead of a new blob. Lookups check it first; it is parsed and validated once at open, and the lookup path still makes no allocation and takes no lock. Files without an overflow are byte-identical to before. Older SDKs reject such a file with `ErrUnsupportedFeature` rather than misread it.
* Signed redirects (on by default; `WithRedirect(false)` turns them off): the sync endpoint may answer with a redirect to a short-lived signed download URL. The SDK fetches it with a fresh request that carries no API key, node ID or other credential header, validates the result exactly like a streamed download, and on any failure repeats the sync once through the endpoint and then stops asking for redirects for an hour. `Synchronizer.RedirectStats` reports counts.
* Redirect target allow-list: a download is followed only if its URL is https, has no user info, uses port 443 and its host is `storage.googleapis.com`, a bucket host under it, or a host you configure with `WithRedirectHosts` or the `ATTOS_REDIRECT_HOSTS` environment variable (comma separated; exact hosts or `*.domain`). The download itself never follows a further redirect. Ordinary redirects of the sync request are limited the same way: same host or an allowed one, never https to http. `WithTestRedirectTargets` lifts the policy for tests against emulators and must not be used in production.
* Polling is jittered: each wait is the interval plus a random share of up to half of it (30 to 45 s at the default), so a fleet that reconnects together does not poll in lockstep. Retry waits after errors and quota pauses are jittered too (a quota pause is never shorter than the server asked).
* Sync requests now send `X-Attos-Capabilities` (`overflow`, `redirect`) and `X-Attos-Accept-Delta: 2`.
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

* v2 format with a compact key index and optional LZ4 block compression.
* Adaptive offset width: 40-bit offsets lift the 4 GiB blob limit.
* Spilling `Builder` for low-memory builds.
* Lower serving RSS: no default warm-up, `MADV_RANDOM`, pages released after the CRC pass. `OpenConfig.Warm` modes (none, index, full).
* Opt-in lean keys: fingerprints instead of stored keys.

## v1.2.0

* Conditional sync (ETag/304), validate-then-swap hot swap, 402/429 back-off, API-key telemetry. Reads format v1 only.
