package attos

// Which hosts a sync redirect may send the SDK to.
//
// A signed download is a 302 from the sync endpoint to a URL the SDK then fetches without
// credentials. The SDK does not trust the sync server's choice blindly: the target must be https, carry no
// userinfo, sit on a default Google Cloud Storage host or on a host the application configured, and the
// download itself must not redirect again. A target that is refused counts as a failed signed download: the sync
// is repeated once through the endpoint (X-Attos-No-Redirect: 1) and the node stops asking for redirects for
// RedirectCooldown, so it is neither fetched from an unexpected host nor re-billed on every poll.

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// RedirectHostsEnv names the environment variable listing extra download hosts, comma separated. An entry is an
// exact host ("dl.example.com") or a subdomain wildcard ("*.example.com", which does not match "example.com").
const RedirectHostsEnv = "ATTOS_REDIRECT_HOSTS"

// defaultRedirectHosts are the Google Cloud Storage endpoints: path-style and virtual-hosted style.
var defaultRedirectHosts = []string{"storage.googleapis.com", "*.storage.googleapis.com"}

type redirectPolicy struct {
	hosts []string // lower-case exact hosts and "*.suffix" wildcards
	// anyTarget (tests only, see WithTestRedirectTargets) also allows http, IP literals, loopback and any host.
	anyTarget bool
}

func newRedirectPolicy(extra []string) *redirectPolicy {
	p := &redirectPolicy{}
	for _, h := range append(append([]string(nil), defaultRedirectHosts...), extra...) {
		if h = normalizeHostPattern(h); h != "" {
			p.hosts = append(p.hosts, h)
		}
	}
	return p
}

// normalizeHostPattern lower-cases a configured entry and drops anything that is not a plain host or wildcard.
func normalizeHostPattern(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" || strings.ContainsAny(h, "/@:?#% \t") || strings.HasSuffix(h, ".") || strings.HasPrefix(h, ".") {
		return ""
	}
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		if rest == "" || strings.Contains(rest, "*") || !strings.Contains(rest, ".") {
			return "" // "*.com" style wildcards are refused
		}
		return h
	}
	if strings.Contains(h, "*") {
		return ""
	}
	return h
}

func parseHostList(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// check reports whether a download may be fetched from u.
func (p *redirectPolicy) check(u *url.URL) error {
	if p.anyTarget {
		if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("unusable redirect target %q", u.Redacted())
		}
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("redirect target %q is not https", u.Redacted())
	}
	if u.User != nil {
		return fmt.Errorf("redirect target %q carries userinfo", u.Redacted())
	}
	if u.Opaque != "" || u.Fragment != "" {
		return fmt.Errorf("redirect target %q is malformed", u.Redacted())
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("redirect target %q has no host", u.Redacted())
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("redirect target %q uses port %s", u.Redacted(), port)
	}
	host = strings.ToLower(host)
	if net.ParseIP(host) != nil || strings.HasSuffix(host, ".") || !isASCIIHost(host) {
		return fmt.Errorf("redirect target %q is not an allowed host", u.Redacted())
	}
	for _, pat := range p.hosts {
		if rest, ok := strings.CutPrefix(pat, "*."); ok {
			if strings.HasSuffix(host, "."+rest) && len(host) > len(rest)+1 {
				return nil
			}
		} else if host == pat {
			return nil
		}
	}
	return fmt.Errorf("redirect host %q is not in the allowed list (set %s or WithRedirectHosts)", host, RedirectHostsEnv)
}

func isASCIIHost(h string) bool {
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// allowsOrdinaryRedirect decides whether an ordinary (unmarked) redirect of the sync request may be followed: never a
// downgrade from https, never userinfo, and only to the same host or an allowed one.
func (p *redirectPolicy) allowsOrdinaryRedirect(next *url.URL, first *url.URL) error {
	if p.anyTarget {
		return nil
	}
	if first.Scheme == "https" && next.Scheme != "https" {
		return fmt.Errorf("redirect from https to %s refused", next.Scheme)
	}
	if next.User != nil {
		return fmt.Errorf("redirect target carries userinfo")
	}
	if strings.EqualFold(next.Host, first.Host) {
		return nil
	}
	return p.check(next)
}
