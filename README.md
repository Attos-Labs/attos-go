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
go get github.com/Attos-Labs/attos-go@main
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

## Errors

- `attos.ErrKeyNotFound`: the key is not in the current dataset.
- `attos.ErrInvalidMagic`, `attos.ErrCorruptedBinary`, `attos.ErrChecksumMismatch`: the blob failed validation.
- `*attos.StatusError`: the cloud returned a non-200 status. `Quota()` is true for plan limits (402/429); the SDK keeps serving the last good blob.
