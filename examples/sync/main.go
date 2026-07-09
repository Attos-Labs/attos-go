package main

import (
	"fmt"
	"log"
	"os"

	"github.com/attos/sdk"
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

	client := attos.NewClient(apiKey)

	fmt.Println("Syncing dataset…")
	if err := client.Sync(datasetID); err != nil {
		log.Fatalf("sync failed: %v", err)
	}
	fmt.Printf("Blob saved to: %s\n", client.BlobPath())

	val, err := client.Get("example-key")
	if err != nil {
		log.Fatalf("lookup failed: %v", err)
	}
	fmt.Printf("Get(\"example-key\") = %s\n", val)
}
