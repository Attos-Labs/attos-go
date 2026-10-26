package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// A minimal RESP2 client: just enough for read-only sampling.
type resp struct {
	conn net.Conn
	rd   *bufio.Reader
}

func dialRedis(addr string, useTLS bool, password string) (*resp, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if useTLS {
		host, _, _ := net.SplitHostPort(addr)
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	r := &resp{conn: conn, rd: bufio.NewReaderSize(conn, 1<<16)}
	if password != "" {
		if _, err := r.do("AUTH", password); err != nil {
			conn.Close()
			return nil, fmt.Errorf("AUTH: %w", err)
		}
	}
	return r, nil
}

func (r *resp) close() { r.conn.Close() }

// do sends one command and returns its reply: string, int64, nil or []any.
func (r *resp) do(args ...string) (any, error) {
	r.conn.SetDeadline(time.Now().Add(30 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(r.conn, b.String()); err != nil {
		return nil, err
	}
	return r.read()
}

func (r *resp) read() (any, error) {
	line, err := r.rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, errors.New("short reply")
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, errors.New(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, _ := strconv.Atoi(body)
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r.rd, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, _ := strconv.Atoi(body)
		if n < 0 {
			return nil, nil
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = r.read(); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

func sampleRedis(c config) (*rows, error) {
	pw := ""
	if c.passEnv != "" {
		pw = os.Getenv(c.passEnv)
	}
	r, err := dialRedis(c.redisAddr, c.redisTLS, pw)
	if err != nil {
		return nil, err
	}
	defer r.close()
	n, err := r.do("DBSIZE")
	if err != nil {
		return nil, err
	}
	total, _ := n.(int64)
	want := c.sample
	if want <= 0 || int64(want) > total {
		want = int(total)
	}
	out := &rows{total: total}
	var memSum float64
	cursor := "0"
	for len(out.sample) < want {
		rep, err := r.do("SCAN", cursor, "COUNT", "1000")
		if err != nil {
			return nil, err
		}
		pair, ok := rep.([]any)
		if !ok || len(pair) != 2 {
			return nil, errors.New("unexpected SCAN reply")
		}
		cursor, _ = pair[0].(string)
		keys, _ := pair[1].([]any)
		for _, k := range keys {
			key, _ := k.(string)
			if len(out.sample) >= want {
				break
			}
			t, err := r.do("TYPE", key)
			if err != nil {
				return nil, err
			}
			if t != "string" {
				out.skipped++
				continue
			}
			v, err := r.do("GET", key)
			if err != nil {
				return nil, err
			}
			val, ok := v.(string)
			if !ok {
				continue // expired between SCAN and GET
			}
			m, err := r.do("MEMORY", "USAGE", key)
			if err != nil {
				return nil, err
			}
			mu, _ := m.(int64)
			memSum += float64(mu)
			out.sample = append(out.sample, [2][]byte{[]byte(key), []byte(val)})
			out.sumKeyLen += float64(len(key))
			out.sumValLen += float64(len(val))
		}
		if cursor == "0" {
			break
		}
	}
	if out.skipped > 0 && len(out.sample) > 0 {
		// Only plain strings can be served; scale the key count by the share seen.
		out.total = int64(float64(total) * float64(len(out.sample)) / float64(len(out.sample)+int(out.skipped)))
	}
	if len(out.sample) > 0 {
		out.redisBytes = memSum / float64(len(out.sample))
	}
	return out, nil
}
