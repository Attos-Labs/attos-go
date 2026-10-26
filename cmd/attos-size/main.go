// Command attos-size estimates what a dataset costs to serve from Attos
// compared with Redis: it samples your data (a CSV in Attos input format, a
// key,value CSV export, or a live Redis read-only), builds real Attos files
// from the sample, and prints the projected file sizes, RAM saved and the
// equivalent Redis monthly cost.
//
//	attos-size data.csv
//	attos-size -kv redis-export.csv
//	attos-size -redis localhost:6379
//
// Live Redis mode only issues read-only commands (SCAN, TYPE, GET, MEMORY USAGE,
// DBSIZE, INFO memory). The Redis price and per-key overhead are assumptions you
// can override; check them against your own bill.
package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"

	attos "github.com/Attos-Labs/attos-go"
)

// assumptions, all overridable on the command line.
const (
	defaultRedisOverhead = 56.0 // bytes per key beyond key+value: dict entry, object header, SDS headers, allocator rounding
	defaultPriceGiBMonth = 25.0 // USD per GiB-month of managed Redis memory (approximate on-demand cache node price)
)

type config struct {
	path        string
	kv          bool
	redisAddr   string
	redisTLS    bool
	passEnv     string
	sample      int
	overhead    float64
	price       float64
	replicas    int
	asJSON      bool
	redisUsedFn func() (int64, int64) // (used_memory, dbsize) for live mode
}

// rows is what the sampler learned about a dataset.
type rows struct {
	total      int64 // rows (or keys) in the whole dataset
	sample     [][2][]byte
	sumKeyLen  float64 // over the sample
	sumValLen  float64
	redisBytes float64 // average MEMORY USAGE per key (live mode only), 0 otherwise
	skipped    int64   // keys that were not strings (live mode)
}

type variant struct {
	Name        string  `json:"name"`
	BytesPerKey float64 `json:"bytes_per_key"`
	FileBytes   int64   `json:"projected_file_bytes"`
	Note        string  `json:"note,omitempty"`
}

type report struct {
	Rows            int64     `json:"rows"`
	SampleRows      int       `json:"sample_rows"`
	Exact           bool      `json:"exact"`
	AvgKeyBytes     float64   `json:"avg_key_bytes"`
	AvgValueBytes   float64   `json:"avg_value_bytes"`
	RedisBytes      int64     `json:"redis_bytes"`
	RedisSource     string    `json:"redis_source"`
	Replicas        int       `json:"replicas"`
	RedisMonthlyUSD float64   `json:"redis_monthly_usd"`
	PriceGiBMonth   float64   `json:"assumed_usd_per_gib_month"`
	Variants        []variant `json:"attos_variants"`
	Skipped         int64     `json:"skipped_non_string_keys,omitempty"`
}

func main() {
	var c config
	flag.BoolVar(&c.kv, "kv", false, "input is a headerless key,value CSV (for example a Redis export)")
	flag.StringVar(&c.redisAddr, "redis", "", "sample a live Redis at host:port (read-only commands)")
	flag.BoolVar(&c.redisTLS, "redis-tls", false, "use TLS for -redis")
	flag.StringVar(&c.passEnv, "redis-password-env", "", "name of an environment variable holding the Redis password")
	flag.IntVar(&c.sample, "sample", 200000, "rows to sample (0 uses every row)")
	flag.Float64Var(&c.overhead, "redis-overhead", defaultRedisOverhead, "assumed Redis bytes per key beyond key and value (file modes)")
	flag.Float64Var(&c.price, "redis-usd-per-gib-month", defaultPriceGiBMonth, "assumed price of one GiB of Redis memory per month")
	flag.IntVar(&c.replicas, "replicas", 0, "Redis replicas, each holding a full copy")
	flag.BoolVar(&c.asJSON, "json", false, "print JSON")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: attos-size [flags] FILE.csv\n       attos-size -redis host:port\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if c.redisAddr == "" {
		if flag.NArg() != 1 {
			flag.Usage()
			os.Exit(2)
		}
		c.path = flag.Arg(0)
	}
	if err := run(c, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "attos-size:", err)
		os.Exit(1)
	}
}

func run(c config, w io.Writer) error {
	var r *rows
	var err error
	source := "estimated from key and value sizes plus an assumed per-key overhead"
	if c.redisAddr != "" {
		r, err = sampleRedis(c)
		source = "measured with MEMORY USAGE on sampled keys"
	} else {
		r, err = sampleFile(c)
	}
	if err != nil {
		return err
	}
	if len(r.sample) == 0 {
		return errors.New("no rows found")
	}
	rep := report{
		Rows: r.total, SampleRows: len(r.sample), Exact: int64(len(r.sample)) == r.total,
		AvgKeyBytes: r.sumKeyLen / float64(len(r.sample)), AvgValueBytes: r.sumValLen / float64(len(r.sample)),
		Replicas: c.replicas, PriceGiBMonth: c.price, RedisSource: source, Skipped: r.skipped,
	}
	perKey := rep.AvgKeyBytes + rep.AvgValueBytes + c.overhead
	if r.redisBytes > 0 {
		perKey = r.redisBytes
	}
	rep.RedisBytes = int64(perKey * float64(r.total) * float64(1+c.replicas))
	rep.RedisMonthlyUSD = float64(rep.RedisBytes) / (1 << 30) * c.price
	rep.Variants, err = buildVariants(r)
	if err != nil {
		return err
	}
	if c.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(w, rep, c)
	return nil
}

func sampleFile(c config) (*rows, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(bufio.NewReaderSize(f, 1<<20))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = false
	var keyCols []int
	valCol := -1
	if !c.kv {
		hdr, err := cr.Read()
		if err != nil {
			return nil, fmt.Errorf("reading header: %w", err)
		}
		type col struct {
			name string
			i    int
		}
		var dims []col
		for i, h := range hdr {
			if h == "attos_value" {
				valCol = i
			} else {
				dims = append(dims, col{h, i})
			}
		}
		if valCol < 0 || len(dims) == 0 {
			return nil, errors.New("CSV needs an attos_value column and at least one key column (or use -kv for key,value files)")
		}
		sort.Slice(dims, func(a, b int) bool { return dims[a].name < dims[b].name })
		for _, d := range dims {
			keyCols = append(keyCols, d.i)
		}
	}
	rng := rand.New(rand.NewSource(1))
	r := &rows{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", r.total+1, err)
		}
		var k, v []byte
		if c.kv {
			if len(rec) < 2 {
				return nil, fmt.Errorf("row %d: expected key,value", r.total+1)
			}
			k, v = []byte(rec[0]), []byte(strings.Join(rec[1:], ","))
		} else {
			parts := make([]string, len(keyCols))
			for i, ci := range keyCols {
				if ci >= len(rec) || valCol >= len(rec) {
					return nil, fmt.Errorf("row %d: too few columns", r.total+1)
				}
				parts[i] = rec[ci]
			}
			k, v = []byte(strings.Join(parts, "\x00")), []byte(rec[valCol])
		}
		r.total++
		if c.sample <= 0 || len(r.sample) < c.sample {
			r.sample = append(r.sample, [2][]byte{k, v})
		} else if j := rng.Int63n(r.total); j < int64(c.sample) {
			r.sample[j] = [2][]byte{k, v}
		}
	}
	for _, kv := range r.sample {
		r.sumKeyLen += float64(len(kv[0]))
		r.sumValLen += float64(len(kv[1]))
	}
	return r, nil
}

func buildVariants(r *rows) ([]variant, error) {
	dir, err := os.MkdirTemp("", "attos-size-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	specs := []struct {
		name string
		note string
		opt  attos.CompileOptions
	}{
		{"Fast (full keys, uncompressed)", "default: zero-allocation lookups, keys verified", attos.CompileOptions{}},
		{"Lean (8-byte key fingerprints)", "smaller; a key that was never stored is wrongly accepted with probability 2^-64", attos.CompileOptions{FingerprintBytes: 8}},
		{"Compressed (LZ4 blocks)", "smallest values; each lookup decompresses a block and allocates", attos.CompileOptions{Compression: attos.CompressLZ4}},
	}
	var out []variant
	for i, s := range specs {
		b := attos.NewBuilder()
		b.Grow(len(r.sample))
		for _, kv := range r.sample {
			if err := b.Add(kv[0], kv[1], 0); err != nil {
				return nil, fmt.Errorf("sample row: %w", err)
			}
		}
		p := fmt.Sprintf("%s/v%d.attos", dir, i)
		if err := b.CompileWithOptions(p, s.opt); err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		bpk := float64(st.Size()) / float64(len(r.sample))
		out = append(out, variant{Name: s.name, BytesPerKey: bpk, FileBytes: int64(bpk * float64(r.total)), Note: s.note})
	}
	return out, nil
}

func gib(b int64) float64 { return float64(b) / (1 << 30) }

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", gib(b))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f KiB", float64(b)/1024)
	}
}

func printReport(w io.Writer, rep report, c config) {
	var b bytes.Buffer
	basis := "extrapolated from a sample"
	if rep.Exact {
		basis = "exact (every row built)"
	}
	fmt.Fprintf(&b, "Dataset: %d keys, %d sampled (%s)\n", rep.Rows, rep.SampleRows, basis)
	fmt.Fprintf(&b, "Average key %.1f B, average value %.1f B\n\n", rep.AvgKeyBytes, rep.AvgValueBytes)
	fmt.Fprintf(&b, "Redis memory: %s", human(rep.RedisBytes))
	if rep.Replicas > 0 {
		fmt.Fprintf(&b, " (including %d replica(s))", rep.Replicas)
	}
	fmt.Fprintf(&b, "\n  basis: %s\n", rep.RedisSource)
	fmt.Fprintf(&b, "  at an assumed $%.2f per GiB-month: about $%.0f per month\n\n", rep.PriceGiBMonth, rep.RedisMonthlyUSD)
	fmt.Fprintf(&b, "%-34s %12s %10s %12s\n", "Attos file", "size", "RAM saved", "vs Redis")
	for _, v := range rep.Variants {
		saved := rep.RedisBytes - v.FileBytes
		pct := 100 * float64(saved) / float64(rep.RedisBytes)
		fmt.Fprintf(&b, "%-34s %12s %9.0f%% %12s\n", v.Name, human(v.FileBytes), pct, fmt.Sprintf("%.1fx smaller", float64(rep.RedisBytes)/float64(v.FileBytes)))
	}
	fmt.Fprintf(&b, "\nAttos files are memory-mapped: one copy per host, shared by every process on it, with no replicas to pay for.\n")
	fmt.Fprintf(&b, "Not included: your Attos plan price, compile time, and network or CPU costs on either side.\n")
	if rep.Skipped > 0 {
		fmt.Fprintf(&b, "Skipped %d sampled keys that are not plain strings (hashes, lists, sets and so on are not representable as a single value).\n", rep.Skipped)
	}
	w.Write(b.Bytes())
	_ = c
}
