package tests

import (
	"os"
	"testing"

	"github.com/Attos-Labs/attos-go"
)

func BenchmarkClientGet(b *testing.B) {
	apiKey := os.Getenv("AT_API_KEY")
	datasetID := os.Getenv("AT_DATASET_ID")

	if apiKey == "" || datasetID == "" {
		b.Skip("Skipping Benchmark; AT_API_KEY or AT_DATASET_ID is not set.")
	}

	client, err := attos.NewSynchronizer(datasetID, attos.WithAPIKey(apiKey))
	if err != nil {
		b.Fatalf("Failed to create synchronizer: %v", err)
	}
	defer client.Close()

	// Make sure the key actually exists
	_, err = client.Get([]byte("key_00000005"))
	if err != nil {
		b.Fatalf("Test key 'key_00000005' not found in dataset: %v", err)
	}

	// Reset the timer so the sync time isn't included in the results
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = client.Get([]byte("key_00000005"))
	}
}

func BenchmarkClientGetConcurrent(b *testing.B) {
	apiKey := os.Getenv("AT_API_KEY")
	datasetID := os.Getenv("AT_DATASET_ID")

	if apiKey == "" || datasetID == "" {
		b.Skip("Skipping Benchmark; AT_API_KEY or AT_DATASET_ID is not set.")
	}

	client, err := attos.NewSynchronizer(datasetID, attos.WithAPIKey(apiKey))
	if err != nil {
		b.Fatalf("Failed to create synchronizer: %v", err)
	}
	defer client.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.Get([]byte("key_00000005"))
			if err != nil {
				b.Errorf("Concurrent Get failed: %v", err)
			}
		}
	})
}
