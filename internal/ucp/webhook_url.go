package ucp

import (
	"errors"
	"fmt"
	"net"
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
// literal private addresses and names that only exist inside a cluster —
// while leaving public endpoints alone.
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

	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsInterfaceLocalMulticast() {
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
