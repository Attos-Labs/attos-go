package attos_test

import (
	"os"
	"testing"
	"time"

	"github.com/attos/sdk"
)

func TestE2ESyncAndGet(t *testing.T) {
	apiKey := os.Getenv("AT_API_KEY")
	datasetID := os.Getenv("AT_DATASET_ID")

	if apiKey == "" || datasetID == "" {
		t.Skip("Skipping E2E test; AT_API_KEY or AT_DATASET_ID is not set.")
	}

	client := attos.NewClient(apiKey)
	defer client.Close()

	t.Logf("Syncing dataset %s...", datasetID)
	start := time.Now()
	err := client.Sync(datasetID)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	t.Logf("Sync complete in %v", time.Since(start))

	// Check a valid key from the 1M dataset
	_, err = client.Get("key_00000005")
	if err != nil {
		t.Fatalf("Get('key_00000005') failed: %v", err)
	}

	// Check another valid key
	_, err = client.Get("key_00000006")
	if err != nil {
		t.Fatalf("Get('key_00000006') failed: %v", err)
	}

	_, err = client.Get("non_existent_key")
	if err == nil {
		t.Errorf("Expected error for non_existent_key, got nil")
	}

	t.Log("All O(1) probe tests passed successfully!")
}
