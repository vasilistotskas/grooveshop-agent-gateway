package ucp

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ErrPlatformURL reports a platform URL the gateway refuses to call.
var ErrPlatformURL = errors.New("ucp: unusable platform url")

// internalSuffixes are hostnames that only resolve inside a cluster.
var internalSuffixes = []string{
	".local",
	".internal",
	".localdomain",
	".cluster.local",
	".svc",
}

// ValidatePlatformURL checks a URL a platform gave us — its profile, the
// order webhook endpoint in it — BEFORE the gateway calls it.
//
// Both arrive on MCP tools that are reachable anonymously (identity is
// optional on /mcp), so an unvalidated value makes the gateway originate
// requests to arbitrary addresses on behalf of an anonymous caller: an
// in-cluster service, a link-local metadata endpoint, or simply a
// blackhole that occupies a worker for its full timeout.
//
// This is a hostname/scheme check, not a DNS check: resolving here would
// add a round trip to every call and would still be time-of-check/
// time-of-use racy. It rejects the reachable shapes — literal special-use
// addresses and names that only exist inside a cluster — while leaving
// public endpoints alone; platformClient checks the address it actually
// connects to.
//
// allowLocal relaxes it to any http(s) host. It is driven by the ENV
// config value: development and the e2e suite legitimately serve
// httptest platforms on 127.0.0.1, while production must never call
// anything but a public https endpoint.
func ValidatePlatformURL(raw string, allowLocal bool) error {
	if raw == "" {
		return fmt.Errorf("%w: missing", ErrPlatformURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a url", ErrPlatformURL)
	}
	// Userinfo hides the real host from a reader: https://a.example@b.
	if u.User != nil {
		return fmt.Errorf("%w: must not carry credentials", ErrPlatformURL)
	}
	if allowLocal {
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf(
				"%w: must be http(s) (got %q)", ErrPlatformURL, u.Scheme)
		}
		if u.Hostname() == "" {
			return fmt.Errorf("%w: missing host", ErrPlatformURL)
		}
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf(
			"%w: must be https (got %q)", ErrPlatformURL, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrPlatformURL)
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(addr) {
			return fmt.Errorf(
				"%w: %s is not publicly routable", ErrPlatformURL, host)
		}
		return nil
	}

	lower := strings.ToLower(host)
	// A single-label name (no dot) only resolves inside a cluster —
	// "backend-service", "redis", "localhost".
	if !strings.Contains(lower, ".") {
		return fmt.Errorf(
			"%w: %q is not a public hostname", ErrPlatformURL, host)
	}
	for _, suffix := range internalSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return fmt.Errorf(
				"%w: %q is a cluster-internal hostname",
				ErrPlatformURL, host)
		}
	}
	return nil
}

// specialUse lists the RFC 6890 special-purpose ranges netip's predicates
// leave out: shared address space, IETF protocol assignments,
// documentation, benchmarking, relays and translation prefixes, and the
// reserved block.
var specialUse = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

// publicAddr reports whether a webhook may be delivered to addr: a global
// unicast address outside every private and special-use range. Loopback,
// link-local (the 169.254.169.254 metadata address included), multicast
// and unspecified addresses are not global unicast.
func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, p := range specialUse {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}
