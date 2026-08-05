package attos_test

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Attos-Labs/attos-go"
)

func generateBenchmarkCSV(b *testing.B, numRecords int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), fmt.Sprintf("bench_%d.csv", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.Write([]string{"dim2", "attos_value", "dim1", "dim3"})

	for i := 0; i < numRecords; i++ {
		w.Write([]string{
			fmt.Sprintf("v2_%d", i),
			fmt.Sprintf("payload_%d", i),
			fmt.Sprintf("v1_%d", i),
			fmt.Sprintf("v3_%d", rand.Intn(100)),
		})
	}
	w.Flush()
	return path
}

func generateBenchmarkNDJSON(b *testing.B, numRecords int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), fmt.Sprintf("bench_%d.jsonl", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	for i := 0; i < numRecords; i++ {
		row := map[string]interface{}{
			"dim2":        fmt.Sprintf("v2_%d", i),
			"attos_value": fmt.Sprintf("payload_%d", i),
			"dim1":        fmt.Sprintf("v1_%d", i),
			"dim3":        fmt.Sprintf("v3_%d", rand.Intn(100)),
		}
		data, _ := json.Marshal(row)
		f.Write(data)
		f.WriteString("\n")
	}
	return path
}

func BenchmarkIngestCSV10M(b *testing.B) {
	csvPath := generateBenchmarkCSV(b, 10_000_000)
	outPath := filepath.Join(b.TempDir(), "out.attos")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		builder := attos.NewBuilder()
		err := builder.BuildFromCSV(csvPath)
		if err != nil {
			b.Fatal(err)
		}
		err = builder.Compile(outPath)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIngestNDJSON10M(b *testing.B) {
	jsonPath := generateBenchmarkNDJSON(b, 10_000_000)
	outPath := filepath.Join(b.TempDir(), "out.attos")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		builder := attos.NewBuilder()
		err := builder.BuildFromNDJSON(jsonPath)
		if err != nil {
			b.Fatal(err)
		}
		err = builder.Compile(outPath)
		if err != nil {
			b.Fatal(err)
		}
	}
}
