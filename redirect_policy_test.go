package attos

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedirectPolicyHosts(t *testing.T) {
	p := newRedirectPolicy([]string{"dl.example.com", "*.cdn.example.net", "BAD/HOST", "*.com", "x*y.com", ""})
	allowed := []string{
		"https://storage.googleapis.com/bkt/obj?X-Goog-Signature=abc&generation=7",
		"https://STORAGE.GOOGLEAPIS.COM/bkt/obj",
		"https://storage.googleapis.com:443/bkt/obj",
		"https://my-bucket.storage.googleapis.com/obj?x=1",
		"https://dl.attoscloud.com/o/datasets/abc/d.v2.attos/1699999999123456?exp=1800000300&sig=abc",
		"https://dl.example.com/o/abc",
		"https://edge.cdn.example.net/o/abc",
		"https://a.b.cdn.example.net/o/abc",
	}
	denied := map[string]string{
		"http://storage.googleapis.com/b/o":             "http scheme",
		"HTTP://storage.googleapis.com/b/o":             "http scheme upper case",
		"//storage.googleapis.com/b/o":                  "no scheme",
		"https://user:pw@storage.googleapis.com/b/o":    "userinfo",
		"https://user@storage.googleapis.com/b/o":       "userinfo without password",
		"https://storage.googleapis.com@evil.com/b/o":   "host confusion via userinfo",
		"https://storage.googleapis.com.evil.com/b/o":   "suffix confusion",
		"https://evil.com/storage.googleapis.com/b/o":   "host in the path",
		"https://evil.com/?storage.googleapis.com":      "host in the query",
		"https://evilstorage.googleapis.com/b/o":        "prefix confusion without a dot",
		"https://storage.googleapis.com.:443/b/o":       "trailing dot",
		"https://storage.googleapis.com:8443/b/o":       "other port",
		"https://127.0.0.1/b/o":                         "IPv4 literal",
		"https://[::1]/b/o":                             "IPv6 literal",
		"https://localhost/b/o":                         "localhost",
		"https://storage.googleapis.com/b/o#frag":       "fragment",
		"https://stоrage.googleapis.com/b/o":            "cyrillic o lookalike",
		"https://googleapis.com/b/o":                    "parent domain",
		"https://evildl.attoscloud.com/o":               "CDN host is exact, not a prefix match",
		"https://x.dl.attoscloud.com/o":                 "CDN host is exact, not a wildcard",
		"https://attoscloud.com/o":                      "apex of the CDN host",
		"http://dl.attoscloud.com/o":                    "CDN host over http",
		"https://.storage.googleapis.com/b/o":           "leading dot (empty label)",
		"https://example.com/b/o":                       "unlisted host",
		"https://cdn.example.net/o":                     "wildcard does not match the apex",
		"https://evil.com/o":                            "unlisted",
		"https://x.com/o":                               "refused wildcard *.com must not allow x.com",
		"https://xay.com/o":                             "refused wildcard x*y.com",
		"https://dl.example.com.evil.com/o":             "configured host as a prefix",
		"https://notdl.example.com/o":                   "configured exact host is not a wildcard",
		"https://bad/host/o":                            "bad configured host dropped",
		"mailto:storage.googleapis.com":                 "opaque URL",
		"https:storage.googleapis.com":                  "opaque https",
		"https://storage.googleapis.com%2eevil.com/b/o": "percent-encoded dot",
		"https://storage.googleapis.com\\@evil.com/b/o": "backslash",
	}
	for _, raw := range allowed {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if err := p.check(u); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	for raw, why := range denied {
		u, err := url.Parse(raw)
		if err != nil {
			continue // unparseable: refused before the policy
		}
		if err := p.check(u); err == nil {
			t.Errorf("%s allowed (%s)", raw, why)
		}
	}
}

func TestRedirectPolicyConfiguration(t *testing.T) {
	t.Setenv(RedirectHostsEnv, " dl.fromenv.example , *.edge.fromenv.example,")
	s := &Synchronizer{}
	WithRedirectHosts("dl.fromoption.example")(s)
	s.policy = newRedirectPolicy(append(parseHostList(envHosts()), s.redirectHosts...))
	for host, want := range map[string]bool{
		"dl.fromenv.example": true, "a.edge.fromenv.example": true, "dl.fromoption.example": true,
		"storage.googleapis.com": true, "evil.example": false, "edge.fromenv.example": false,
	} {
		u, _ := url.Parse("https://" + host + "/o")
		if got := s.policy.check(u) == nil; got != want {
			t.Errorf("%s: allowed=%v, want %v", host, got, want)
		}
	}
	// the test option lifts the policy but still needs a real http(s) URL
	any := &redirectPolicy{anyTarget: true}
	for raw, want := range map[string]bool{"http://127.0.0.1:9/x": true, "https://anything.test/x": true, "ftp://x/y": false, "https:///x": false} {
		u, _ := url.Parse(raw)
		if got := any.check(u) == nil; got != want {
			t.Errorf("test policy %s: %v", raw, got)
		}
	}
}

func envHosts() string { return strings.Join(parseHostList(os.Getenv(RedirectHostsEnv)), ",") }

// tlsClient returns an http.Client that sends connections for *.example.com to e.store (trusting its certificate), so
// a "https://example.com/..." download lands on the test server; every other address is dialled normally.
func (e *redirectEnv) tlsClient() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(e.store.Certificate())
	addr := e.store.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(target); err == nil && (host == "example.com" || strings.HasSuffix(host, ".example.com")) {
				target = addr
			}
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}, Timeout: 20 * time.Second}
}

func TestRedirectToConfiguredCDNHost(t *testing.T) {
	for name, opt := range map[string]func(t *testing.T) Option{
		"option": func(t *testing.T) Option { return WithRedirectHosts("example.com") },
		"env":    func(t *testing.T) Option { t.Setenv(RedirectHostsEnv, "example.com"); return func(*Synchronizer) {} },
		"wild":   func(t *testing.T) Option { return WithRedirectHosts("*.example.com") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newRedirectEnvTLS(t, buildBlob(t, "a", 200), true)
			host := "example.com"
			if name == "wild" {
				host = "dl.example.com"
			}
			e.location = "https://" + host + "/obj?exp=1&sig=abc"
			s := e.syncStrict(t, WithHTTPClient(e.tlsClient()), opt(t))
			wantValue(t, s, "key_1", "a_1")
			if r, fb := s.RedirectStats(); r != 1 || fb != 0 {
				t.Fatalf("redirected=%d fallbacks=%d", r, fb)
			}
		})
	}
}

// A target outside the policy is never fetched: the sync is repeated once through the endpoint, and the node then
// stops asking for redirects, so the bytes are neither fetched from the odd host nor re-billed on every poll.
func TestRedirectToDisallowedHostFallsBackOnce(t *testing.T) {
	cases := map[string]string{
		"http scheme":       "http://example.com/obj",
		"userinfo":          "https://user:pw@example.com/obj",
		"confusion suffix":  "https://storage.googleapis.com.evil.com/obj",
		"confusion at-sign": "https://storage.googleapis.com@evil.com/obj",
		"host in path":      "https://evil.com/storage.googleapis.com/obj",
		"unlisted host":     "https://example.org/obj",
		"other port":        "https://example.com:8443/obj",
	}
	for name, loc := range cases {
		t.Run(name, func(t *testing.T) {
			blob := buildBlob(t, "a", 200)
			e := newRedirectEnvTLS(t, blob, true)
			e.location = loc
			now := time.Unix(7_000_000, 0)
			s := e.syncStrict(t, WithHTTPClient(e.tlsClient()), WithRedirectHosts("example.com"), withClock(func() time.Time { return now }))
			wantValue(t, s, "key_2", "a_2")
			if r, fb := s.RedirectStats(); r != 0 || fb != 1 {
				t.Fatalf("redirected=%d fallbacks=%d", r, fb)
			}
			if e.nStore() != 0 {
				t.Fatalf("the refused target (or the store) was contacted %d times", e.nStore())
			}
			if e.nAPI() != 2 || e.lastAPI().Get(noRedirectHeader) != "1" {
				t.Fatalf("api requests %d, last headers %v", e.nAPI(), e.lastAPI())
			}
			// the next sync does not ask again
			e.publish(buildBlob(t, "b", 200), nil)
			now = now.Add(time.Minute)
			if err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(e.lastAPI().Get(capabilitiesHeader), "redirect") || e.nAPI() != 3 {
				t.Fatalf("redirect asked again during the cool-down: %v (%d requests)", e.lastAPI(), e.nAPI())
			}
		})
	}
}

// The signed download itself must not redirect: a 3xx from the download host is a failed download, never followed.
func TestSignedDownloadDoesNotFollowRedirects(t *testing.T) {
	e := newRedirectEnvTLS(t, buildBlob(t, "a", 200), true)
	hits := 0
	e.storeMod = func(w http.ResponseWriter, body []byte) {
		hits++
		http.Redirect(w, &http.Request{}, "https://example.com/elsewhere", http.StatusFound)
	}
	e.location = "https://example.com/obj"
	s := e.syncStrict(t, WithHTTPClient(e.tlsClient()), WithRedirectHosts("example.com"))
	wantValue(t, s, "key_3", "a_3")
	if hits != 1 {
		t.Fatalf("the download host was hit %d times (a chain was followed)", hits)
	}
	if r, fb := s.RedirectStats(); r != 0 || fb != 1 {
		t.Fatalf("redirected=%d fallbacks=%d", r, fb)
	}
}

// Ordinary redirects of the sync request: same host or an allowed one, never https -> http, never userinfo.
func TestOrdinaryRedirectPolicy(t *testing.T) {
	p := newRedirectPolicy([]string{"dl.example.com"})
	first, _ := url.Parse("https://api.attoscloud.com/api/v1/datasets/sync/x")
	for raw, want := range map[string]bool{
		"https://api.attoscloud.com/other":     true,  // same host
		"https://API.attoscloud.com/other":     true,  // same host, case
		"https://dl.example.com/x":             true,  // configured
		"https://storage.googleapis.com/b/o":   true,  // default
		"http://api.attoscloud.com/other":      false, // downgrade
		"https://u:p@api.attoscloud.com/other": false, // userinfo
		"https://evil.com/x":                   false,
	} {
		u, _ := url.Parse(raw)
		if got := p.allowsOrdinaryRedirect(u, first) == nil; got != want {
			t.Errorf("%s: allowed=%v, want %v", raw, got, want)
		}
	}
	httpFirst, _ := url.Parse("http://127.0.0.1:8080/sync")
	if u, _ := url.Parse("http://127.0.0.1:8080/other"); p.allowsOrdinaryRedirect(u, httpFirst) != nil {
		t.Error("a same-host redirect on an http base URL (development) must work")
	}
}
