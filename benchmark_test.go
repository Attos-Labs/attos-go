package attos_test

import (
	"os"
	"testing"

	"github.com/attos/sdk"
)

func BenchmarkClientGet(b *testing.B) {
	apiKey := os.Getenv("AT_API_KEY")
	datasetID := os.Getenv("AT_DATASET_ID")

	if apiKey == "" || datasetID == "" {
		b.Skip("Skipping Benchmark; AT_API_KEY or AT_DATASET_ID is not set.")
	}

	client := attos.NewClient(apiKey)
	defer client.Close()

	// Sync the dataset once before starting the benchmark timer
	err := client.Sync(datasetID)
	if err != nil {
		b.Fatalf("Sync failed: %v", err)
	}

	// Make sure the key actually exists
	_, err = client.Get("key_00000005")
	if err != nil {
		b.Fatalf("Test key 'key_00000005' not found in dataset: %v", err)
	}

	// Reset the timer so the sync time isn't included in the results
	b.ResetTimer()

	// b.N is dynamically scaled by Go to run millions of times
	for i := 0; i < b.N; i++ {
		_, _ = client.Get("key_00000005")
	}
}

func BenchmarkClientGetConcurrent(b *testing.B) {
	apiKey := os.Getenv("AT_API_KEY")
	datasetID := os.Getenv("AT_DATASET_ID")

	if apiKey == "" || datasetID == "" {
		b.Skip("Skipping Benchmark; AT_API_KEY or AT_DATASET_ID is not set.")
	}

	client := attos.NewClient(apiKey)
	defer client.Close()

	if err := client.Sync(datasetID); err != nil {
		b.Fatalf("Sync failed: %v", err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.Get("key_00000005")
			if err != nil {
				b.Errorf("Concurrent Get failed: %v", err)
			}
		}
	})
}
