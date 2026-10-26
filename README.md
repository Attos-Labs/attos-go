# attos-go

`attos-go` is the Go client SDK for Attos, a zero-latency state distribution engine. Attos Cloud compiles your dataset into a static `.attos` blob. This SDK downloads it, memory-maps it, and serves lookups in-process: lock-free and allocation-free on the read path.

## 3-minute quickstart

You need Go 1.24+ and a free Attos Cloud account.

**1. Sign up** at <https://attoscloud.com/auth/signup>. Every signup gets a Free-tier personal project.

**2. Create a dataset.** Save this as `sample.ndjson`:

```json
{"user_id": "user_1", "attos_value": "free"}
{"user_id": "user_2", "attos_value": "pro"}
{"user_id": "user_3", "attos_value": "enterprise"}
```

In the dashboard open **Datasets → Create dataset** and upload the file. When the status is `ready`, copy the dataset ID shown under the dataset name.

**3. Create an API key** under **Settings → API Keys**. Copy it now: it is only shown once.  New keys start with `at_live_`; keys created earlier start with `nh_live_` and still work.

**4. Look up a key.**

```bash
mkdir attos-demo && cd attos-demo
go mod init attos-demo
go get github.com/Attos-Labs/attos-go@latest
export ATTOS_API_KEY="at_live_..."
export ATTOS_DATASET_ID="<dataset id from step 2>"
```

```go
// main.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	attos "github.com/Attos-Labs/attos-go"
)

func main() {
	s, err := attos.NewSynchronizer(
		context.Background(),
		os.Getenv("ATTOS_DATASET_ID"),
		attos.WithBaseURL("https://api.attoscloud.com"),
		attos.WithAPIKey(os.Getenv("ATTOS_API_KEY")),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	v, err := s.GetString("user_2")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("user_2 =", v)
}
```

```bash
go run .
# user_2 = pro
```

`NewSynchronizer` downloads the blob before it returns, so the first `Get` already works. The blob is cached in `./.attos/cache`, and the SDK re-checks for a new version every 30 seconds in the background.

If it fails: `401` means the key is wrong, `404` means the dataset ID is wrong, and `409` means the dataset is still compiling, so retry in a few seconds.

## Create datasets via the API

You can also create a dataset without the dashboard. Upload a CSV or NDJSON file with your API key:

```bash
curl -X POST https://api.attoscloud.com/api/v1/datasets/compile \
  -H "X-API-Key: $ATTOS_API_KEY" \
  -F file=@sample.ndjson \
  -F name=my-dataset
```

The input must have an `attos_value` column; every other column is a key dimension. The response is `201` with `id` (use it as `ATTOS_DATASET_ID`), `name`, `row_count`, `blob_size`, `compression`, `compression_note` and `status`. `name` is optional and defaults to the file name without its extension. Instead of a multipart upload you can send the NDJSON as the request body and set the name with an `X-Dataset-Name` header.

Compression is off by default. To turn on LZ4, add `-F compression=lz4` and optionally `-F block_size=<records>` (for a raw body, use the `X-Attos-Compression` and `X-Attos-Block-Size` headers). With LZ4, each lookup decompresses one block, which costs roughly 0.4 µs per record in the block and allocates. There is no setting for the input format or the blob format version. To replace the data of an existing dataset, `PUT /api/v1/datasets/{id}/data` with the same API key; it counts against your plan's monthly update limit.

Listing and deleting datasets and creating API keys need a dashboard login and are not available with an API key.

## Local files

You can also map a `.attos` file directly, with no cloud involved:

```go
m, err := attos.Open("/var/data/rules.attos")
if err != nil {
	log.Fatal(err)
}
defer m.Close()

val, err := m.GetString("user_123")
```

## Cloud sync options

Pass these to `attos.NewSynchronizer(ctx, datasetID, opts...)`:

| Option | Description |
| :--- | :--- |
| `WithBaseURL(string)` | Control plane URL. Default `http://localhost:8080`, so always set it for Attos Cloud. |
| `WithAPIKey(string)` | API key, sent as `X-API-Key`. |
| `WithSyncInterval(time.Duration)` | Poll interval. Default 30s; `0` disables background polling. |
| `WithCacheDir(string)` | Where the downloaded blob is cached. Default `./.attos/cache`. |
| `WithNodeID(string)` | Node ID reported to the cloud. Defaults to `ATTOS_NODE_ID`, then the hostname. Nodes are counted by distinct ID. |
| `WithCompression(bool)` | Ask for the gzip transfer copy. Default `true`. |
| `WithLogger(*log.Logger)` | Logger for sync and telemetry messages. |
| `WithTelemetryDisabled()` | Turn the telemetry emitter off. |
| `WithOpenConfig(attos.OpenConfig)` | Checksum validation and memory pinning for the mapped blob. |

## Batch lookups and typed values

```go
out := make([][]byte, len(keys))
found, err := m.GetMany(keys, out) // out[i] == nil when keys[i] is absent
```

`GetMany` pins the mapping once and reads a group of keys' table entries together, so it costs a little less per key than repeated `Get` on large files (measured at roughly 6-10 % on the 15M-key benchmark file; `ATTOS_BENCH_FILE=your.attos go test -run xxx -bench 'Single|Batch'` measures your own). Batch calls count as lookups but are not latency-sampled, and make no allocations.

Values are opaque bytes. If yours are structured, `Schema` reads fixed layouts (big-endian numbers, `u16`-length strings) straight from the value, using the same layout as the Rust and Java SDKs:

```go
sc := attos.NewSchema(attos.Field{"id", attos.U64}, attos.Field{"name", attos.String})
name, err := sc.String(value, "name")
```

## Metrics

`http.Handle("/metrics", attos.MetricsHandler(sync))` serves Prometheus text: lookups, misses and hit ratio, a sampled lookup-latency histogram (1 in 256 lookups), hot swaps, CRC and sync errors, and `attos_file_info{format_version,etag,stash}` for the file in use. Scraping reads counters the SDK keeps anyway; lookups stay allocation-free.

## Sizing against Redis: `attos-size`

```bash
go run github.com/Attos-Labs/attos-go/cmd/attos-size data.csv          # Attos input format (attos_value column)
go run github.com/Attos-Labs/attos-go/cmd/attos-size -kv export.csv    # headerless key,value CSV, e.g. a Redis export
go run github.com/Attos-Labs/attos-go/cmd/attos-size -redis host:6379  # sample a live Redis (read-only commands only)
```

It samples up to 200,000 rows (`-sample`), builds real Attos files from the sample in three variants (Fast, Lean, Compressed), extrapolates the file size, and prints RAM saved against Redis and the equivalent monthly Redis cost. For file input the Redis size is an estimate (key + value + an assumed 56 bytes per key, `-redis-overhead`); in live mode it is measured with `MEMORY USAGE`. The price (`-redis-usd-per-gib-month`, default 25) and `-replicas` are assumptions you should replace with your own bill. Use `-json` for machine-readable output. Live mode sends only `SCAN`, `TYPE`, `GET`, `MEMORY USAGE` and `DBSIZE`; the password is read from the environment variable named by `-redis-password-env`.

## Signed artifacts

```go
pub, _ := attos.ParsePublicKey("<base64 Ed25519 public key>")
s, err := attos.NewSynchronizer(ctx, stashID, attos.WithSignatureKeys(map[string]ed25519.PublicKey{"2026-10": pub}))
```

With keys set, the synchronizer installs a file only if its `X-Attos-Signature` header verifies (Ed25519 over the file's SHA-256, bound to the stash id), and re-verifies the cached copy on restart. A missing or invalid signature is refused (`ErrSignatureMissing`, `ErrSignatureInvalid`), the current file keeps serving, and `attos_signature_errors_total` counts it. The file format is unchanged. The control plane must sign the files for this to work: see SIGNED_ARTIFACTS.md.

## Errors

- `attos.ErrKeyNotFound`: the key is not in the current dataset.
- `attos.ErrInvalidMagic`, `attos.ErrCorruptedBinary`, `attos.ErrChecksumMismatch`: the blob failed validation.
- `*attos.StatusError`: the cloud returned a non-200 status. `Quota()` is true for plan limits (402/429); the SDK keeps serving the last good blob.

## Format compatibility

This SDK reads `.attos` format versions 1 and 2 (PTHash, LZ4 blocks, 40-bit offsets, lean keys) from the first release that contains them, and fails closed with an error that names the unsupported feature and says to upgrade the SDK when it meets a newer file. The full table of format features against SDK versions is in [docs/FORMAT_COMPATIBILITY.md](https://github.com/Attos-Labs/attos-cloud/blob/main/docs/FORMAT_COMPATIBILITY.md).
