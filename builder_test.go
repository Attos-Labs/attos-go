package attos_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Attos-Labs/attos-go"
)

func TestBuilderCSV(t *testing.T) {
	csvPath := filepath.Join(t.TempDir(), "test.csv")
	err := os.WriteFile(csvPath, []byte("z,a,attos_value,b\nvalZ,valA,payload_1,valB\nfooZ,fooA,payload_2,fooB\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	builder := attos.NewBuilder()
	if err := builder.BuildFromCSV(csvPath); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "out.attos")
	if err := builder.Compile(outPath); err != nil {
		t.Fatal(err)
	}

	m, err := attos.OpenWithConfig(outPath, attos.OpenConfig{ValidateChecksum: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Keys are: a, b, z (sorted).
	// Row 1: valA\x00valB\x00valZ -> payload_1
	// Row 2: fooA\x00fooB\x00fooZ -> payload_2

	val, err := m.GetString("valA\x00valB\x00valZ")
	if err != nil {
		t.Fatal(err)
	}
	if val != "payload_1" {
		t.Errorf("expected payload_1, got %s", val)
	}

	val2, err := m.GetString("fooA\x00fooB\x00fooZ")
	if err != nil {
		t.Fatal(err)
	}
	if val2 != "payload_2" {
		t.Errorf("expected payload_2, got %s", val2)
	}
}

func TestBuilderNDJSON(t *testing.T) {
	jsonPath := filepath.Join(t.TempDir(), "test.jsonl")
	err := os.WriteFile(jsonPath, []byte(`{"z":"valZ","a":"valA","attos_value":"payload_1","b":"valB"}
{"z":"fooZ","a":"fooA","attos_value":"payload_2","b":"fooB"}
`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	builder := attos.NewBuilder()
	if err := builder.BuildFromNDJSON(jsonPath); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "out.attos")
	if err := builder.Compile(outPath); err != nil {
		t.Fatal(err)
	}

	m, err := attos.OpenWithConfig(outPath, attos.OpenConfig{ValidateChecksum: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	val, err := m.GetString("valA\x00valB\x00valZ")
	if err != nil {
		t.Fatal(err)
	}
	if val != "payload_1" {
		t.Errorf("expected payload_1, got %s", val)
	}

	val2, err := m.GetString("fooA\x00fooB\x00fooZ")
	if err != nil {
		t.Fatal(err)
	}
	if val2 != "payload_2" {
		t.Errorf("expected payload_2, got %s", val2)
	}
}
