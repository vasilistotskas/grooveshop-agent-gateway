package ucp

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// platformClient calls URLs a platform controls — its profile, its
// webhook endpoint. UCP's fetch-safety rules apply to both: never follow
// a redirect, and connect only to public addresses. allowLocal is the
// ENV-driven AllowLocalWebhooks: only development and tests may reach
// loopback or private addresses.
//
// The address check runs on what is actually dialled, not on the URL's
// text: a public name can resolve to a private address, or rebind to one
// after it was vetted.
func platformClient(allowLocal bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowLocal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || !publicAddr(addr) {
				return fmt.Errorf("%w: %s is not publicly routable",
					ErrPlatformURL, host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		// A redirect would carry the request to a target no check ever
		// saw — an in-cluster service or the metadata address, over plain
		// http. A 3xx is a failure.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			// No proxy: the dial check must see the platform's address.
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConnsPerHost: deliveryWorkers,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}
