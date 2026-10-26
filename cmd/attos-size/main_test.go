package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, header string, n int, row func(i int) string) string {
	t.Helper()
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	for i := 0; i < n; i++ {
		b.WriteString(row(i) + "\n")
	}
	p := filepath.Join(t.TempDir(), "in.csv")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileExactAndSampled(t *testing.T) {
	p := writeCSV(t, "id,region,attos_value", 5000, func(i int) string {
		return fmt.Sprintf("user%d,eu,value-%d-%s", i, i, strings.Repeat("x", 40))
	})
	var out bytes.Buffer
	c := config{path: p, sample: 0, overhead: defaultRedisOverhead, price: defaultPriceGiBMonth, asJSON: true}
	if err := run(c, &out); err != nil {
		t.Fatal(err)
	}
	var exact report
	if err := json.Unmarshal(out.Bytes(), &exact); err != nil {
		t.Fatal(err)
	}
	if !exact.Exact || exact.Rows != 5000 || len(exact.Variants) != 3 {
		t.Fatalf("%+v", exact)
	}
	fast, lean := exact.Variants[0], exact.Variants[1]
	if fast.FileBytes <= 5000*40 || lean.FileBytes >= fast.FileBytes {
		t.Fatalf("fast=%d lean=%d", fast.FileBytes, lean.FileBytes)
	}
	if exact.RedisBytes <= fast.FileBytes {
		t.Fatalf("redis %d should exceed file %d", exact.RedisBytes, fast.FileBytes)
	}
	// A 1000-row sample extrapolates to within 5 % of the exact build.
	out.Reset()
	c.sample = 1000
	if err := run(c, &out); err != nil {
		t.Fatal(err)
	}
	var est report
	_ = json.Unmarshal(out.Bytes(), &est)
	if est.Exact || est.SampleRows != 1000 {
		t.Fatalf("%+v", est)
	}
	d := float64(est.Variants[0].FileBytes-fast.FileBytes) / float64(fast.FileBytes)
	if d > 0.05 || d < -0.05 {
		t.Fatalf("extrapolation off by %.1f%%", 100*d)
	}
	// text output mentions every variant
	out.Reset()
	c.asJSON = false
	if err := run(c, &out); err != nil || !strings.Contains(out.String(), "Lean") || !strings.Contains(out.String(), "RAM saved") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestKVAndBadInput(t *testing.T) {
	p := writeCSV(t, "", 300, func(i int) string { return fmt.Sprintf("k%d,v%d", i, i) })
	var out bytes.Buffer
	if err := run(config{path: p, kv: true, overhead: 56, price: 25, asJSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	bad := writeCSV(t, "id,name", 3, func(i int) string { return "a,b" })
	if err := run(config{path: bad, overhead: 56, price: 25}, &out); err == nil {
		t.Fatal("missing attos_value column should fail")
	}
	empty := writeCSV(t, "id,attos_value", 0, nil)
	if err := run(config{path: empty, overhead: 56, price: 25}, &out); err == nil {
		t.Fatal("empty file should fail")
	}
}

// fakeRedis answers the read-only commands attos-size uses.
func fakeRedis(t *testing.T, n int) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					cnt, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					args := make([]string, cnt)
					for i := range args {
						l1, _ := rd.ReadString('\n')
						ln, _ := strconv.Atoi(strings.TrimSpace(l1[1:]))
						buf := make([]byte, ln+2)
						_, _ = rd.Read(buf[:0])
						for read := 0; read < ln+2; {
							k, err := rd.Read(buf[read:])
							if err != nil {
								return
							}
							read += k
						}
						args[i] = string(buf[:ln])
					}
					switch strings.ToUpper(args[0]) {
					case "DBSIZE":
						fmt.Fprintf(c, ":%d\r\n", n+1)
					case "SCAN":
						fmt.Fprint(c, "*2\r\n$1\r\n0\r\n")
						fmt.Fprintf(c, "*%d\r\n", n+1)
						for i := 0; i < n; i++ {
							k := fmt.Sprintf("key:%d", i)
							fmt.Fprintf(c, "$%d\r\n%s\r\n", len(k), k)
						}
						fmt.Fprint(c, "$4\r\nhash\r\n") // a non-string key
					case "TYPE":
						if args[1] == "hash" {
							fmt.Fprint(c, "+hash\r\n")
						} else {
							fmt.Fprint(c, "+string\r\n")
						}
					case "GET":
						v := "value-" + args[1]
						fmt.Fprintf(c, "$%d\r\n%s\r\n", len(v), v)
					case "MEMORY":
						fmt.Fprint(c, ":120\r\n")
					default:
						fmt.Fprint(c, "-ERR unknown\r\n")
					}
				}
			}(c)
		}
	}()
	return l.Addr().String()
}

func TestLiveRedis(t *testing.T) {
	addr := fakeRedis(t, 200)
	var out bytes.Buffer
	if err := run(config{redisAddr: addr, sample: 0, overhead: 56, price: 25, asJSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var rep report
	_ = json.Unmarshal(out.Bytes(), &rep)
	if rep.SampleRows != 200 || rep.Skipped != 1 || !strings.Contains(rep.RedisSource, "MEMORY USAGE") {
		t.Fatalf("%+v", rep)
	}
	if rep.RedisBytes < 200*110 || rep.RedisBytes > 201*130 {
		t.Fatalf("redis bytes %d", rep.RedisBytes)
	}
}
