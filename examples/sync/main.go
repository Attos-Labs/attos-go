package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Attos-Labs/attos-go"
)

func main() {
	apiKey := os.Getenv("NH_API_KEY")
	if apiKey == "" {
		log.Fatal("set NH_API_KEY environment variable")
	}

	datasetID := os.Getenv("NH_DATASET_ID")
	if datasetID == "" {
		log.Fatal("set NH_DATASET_ID environment variable")
	}

	ctx := context.Background()
	client, err := attos.NewSynchronizer(ctx, datasetID, attos.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("failed to initialize synchronizer: %v", err)
	}
	defer client.Close()

	fmt.Println("Syncing dataset…")
	// Sync is called internally by NewSynchronizer, but we can call it again if we want.
	if err := client.Sync(ctx); err != nil {
		log.Fatalf("sync failed: %v", err)
	}
	fmt.Printf("Blob saved.\n")

	val, err := client.Get([]byte("example-key"))
	if err != nil {
		log.Fatalf("lookup failed: %v", err)
	}
	fmt.Printf("Get(\"example-key\") = %s\n", val)
}
