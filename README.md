# attos-go

`attos-go` is the ultra-low-latency Go client SDK for the Attos Zero-Latency State Distribution Engine. It achieves sub-150ns data lookups via lock-free memory mapping of static state, allowing massive-scale edge deployment with strictly zero-allocation reads.

## Features

- **Sub-150ns lookups**: Direct pointer arithmetic over pre-compiled routing tables.
- **Zero-allocation hot path**: Operates at exactly `0 B/op` overhead on read queries.
- **Lock-free background syncing**: Employs `sync/atomic` pointer hot-swapping to update state safely without blocking readers.
- **Zero external dependencies**: Built entirely with standard Go libraries.

## Installation

```bash
go get github.com/Attos-Labs/attos-go@v0.1.0
```

## Quick Start

### Static Local Usage
Instantly map and query dataset blobs directly from the local filesystem with zero I/O latency on read.

```go
package main

import (
	"fmt"
	"log"

	"github.com/Attos-Labs/attos-go"
)

func main() {
	db, err := attos.Open("/var/data/rules.attos")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	val, err := db.GetString("user_123")
	if err != nil {
		log.Fatal(err)
	}
	
	fmt.Println("Value:", val)
}
```

### Cloud-Synchronized Usage
Attach the SDK to your Attos Cloud API for real-time background polling and seamless atomic hot-swapping.

```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/Attos-Labs/attos-go"
)

func main() {
	client, err := attos.NewSynchronizer(
		"dataset_xyz",
		attos.WithAPIKey("sk_live_..."),
		attos.WithSyncInterval(10 * time.Minute),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	val, err := client.Get([]byte("user_123"))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Value:", string(val))
}
```

## Configuration Options

When initializing a cloud-synchronized client via `attos.NewSynchronizer`, you can pass the following functional options:

| Option | Description |
| :--- | :--- |
| `WithAPIKey(string)` | Sets the API key used to authenticate with the Attos Cloud platform. |
| `WithSyncInterval(time.Duration)` | Sets the background polling interval. The SDK will automatically fetch and swap to new dataset versions. |
| `WithStoragePath(string)` | Customizes the local directory where downloaded `.attos` binary blobs are cached. |

## Errors Reference

When interacting with the SDK, you may encounter the following domain-specific errors:

- `attos.ErrKeyNotFound`: Returned when the requested key does not exist within the current routing table.
- `attos.ErrInvalidBinary`: Returned during initialization if the `.attos` file signature is corrupt or improperly formatted.
- `attos.ErrSyncFailed`: Returned when the background synchronizer fails to communicate with the Attos Cloud API.
