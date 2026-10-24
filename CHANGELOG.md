# Changelog

## Unreleased

* Fail closed with an explicit error when a file uses a format feature newer than the SDK: `*FeatureError` (`errors.Is(err, ErrUnsupportedFeature)`) for unknown flag bits, offset widths and fingerprint widths on an intact file; `VersionError` now says to upgrade.

## v1.3.0 (2026-10-22)

* PTHash v2 format with a minimal perfect hash and optional LZ4 block compression.
* Adaptive offset width: 40-bit offsets lift the 4 GiB blob limit.
* Spilling `Builder` for low-memory builds.
* Lower serving RSS: no default warm-up, `MADV_RANDOM`, pages released after the CRC pass. `OpenConfig.Warm` modes (none, index, full).
* Opt-in lean keys: fingerprints instead of stored keys.

## v1.2.0

* Conditional sync (ETag/304), validate-then-swap hot swap, 402/429 back-off, API-key telemetry. Reads format v1 only.
