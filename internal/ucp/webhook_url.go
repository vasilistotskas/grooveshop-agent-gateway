package ucp

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// ErrWebhookURL reports a webhook endpoint the gateway refuses to call.
var ErrWebhookURL = errors.New("ucp: unusable webhook url")

// internalSuffixes are hostnames that only resolve inside a cluster.
var internalSuffixes = []string{
	".local",
	".internal",
	".localdomain",
	".cluster.local",
	".svc",
}

// ValidateWebhookURL checks an endpoint BEFORE it is stored on a session.
//
// webhookUrl arrives on the create_checkout MCP tool, which is reachable
// anonymously — identity is optional on /mcp. Whatever is stored here is
// later POSTed to by the dispatcher on every order transition, so an
// unvalidated value makes the gateway originate requests to arbitrary
// addresses on behalf of an anonymous caller: an in-cluster service, a
// link-local metadata endpoint, or simply a blackhole that occupies a
// delivery worker for the full retry budget.
//
// Validation happens at registration rather than at delivery so the
// caller gets an actionable error instead of a silent non-delivery, and
// so a bad value never reaches the queue at all.
//
// This is a hostname/scheme check, not a DNS check: resolving here would
// add a round trip to every checkout and would still be
// time-of-check/time-of-use racy. It rejects the reachable shapes —
// literal special-use addresses and names that only exist inside a
// cluster — while leaving public endpoints alone; the dispatcher checks
// the address it actually connects to.
//
// allowLocal relaxes it to any http(s) host. It is driven by the ENV
// config value: development and the e2e suite legitimately register
// httptest servers on 127.0.0.1, while production must never call
// anything but a public https endpoint.
func ValidateWebhookURL(raw string, allowLocal bool) error {
	if raw == "" {
		return nil // no endpoint registered — nothing to deliver
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: not a url", ErrWebhookURL)
	}
	if allowLocal {
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf(
				"%w: must be http(s) (got %q)", ErrWebhookURL, u.Scheme)
		}
		if u.Hostname() == "" {
			return fmt.Errorf("%w: missing host", ErrWebhookURL)
		}
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf(
			"%w: must be https (got %q)", ErrWebhookURL, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrWebhookURL)
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(addr) {
			return fmt.Errorf(
				"%w: %s is not publicly routable", ErrWebhookURL, host)
		}
		return nil
	}

	lower := strings.ToLower(host)
	// A single-label name (no dot) only resolves inside a cluster —
	// "backend-service", "redis", "localhost".
	if !strings.Contains(lower, ".") {
		return fmt.Errorf(
			"%w: %q is not a public hostname", ErrWebhookURL, host)
	}
	for _, suffix := range internalSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return fmt.Errorf(
				"%w: %q is a cluster-internal hostname",
				ErrWebhookURL, host)
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
