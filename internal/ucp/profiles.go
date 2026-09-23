package ucp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// Discovery error codes (overview, Negotiation Errors).
const (
	CodeInvalidProfileURL  = "invalid_profile_url"
	CodeProfileUnreachable = "profile_unreachable"
	CodeProfileMalformed   = "profile_malformed"
	CodeVersionUnsupported = "version_unsupported"
)

// DiscoveryError is a failure to obtain a usable platform profile. The
// spec makes it a transport error, not a business outcome.
type DiscoveryError struct {
	Code    string
	Content string
}

func (e *DiscoveryError) Error() string {
	return "ucp: " + e.Code + ": " + e.Content
}

func discoveryErr(code, format string, args ...any) *DiscoveryError {
	return &DiscoveryError{Code: code, Content: fmt.Sprintf(format, args...)}
}

// Profile fetch bounds (overview, Profile Requirements — Fetching).
const (
	// profileTTLFloor is the spec's minimum cache lifetime, whatever the
	// origin's Cache-Control says; profileTTLCeiling bounds how long a
	// changed profile can go unnoticed.
	profileTTLFloor   = time.Minute
	profileTTLCeiling = time.Hour
	// failureBackoff caches a failed discovery, so a persistently broken
	// or malicious endpoint is not re-fetched on every request.
	failureBackoff = 30 * time.Second
	// maxProfileBytes bounds the response; the spec asks for no less
	// than 128 KiB so conformant profiles are never rejected.
	maxProfileBytes = 256 << 10
	// maxProfiles is the fixed discovery footprint: the cache cannot grow
	// with the number of distinct profile URLs callers send.
	maxProfiles = 1024
	// fetchTimeout covers connect, TLS and body; fetchRate and fetchBurst
	// are the global discovery budget across every tenant.
	fetchTimeout = 5 * time.Second
	fetchRate    = 20
	fetchBurst   = 40
)

// ProfileResolver fetches, validates and caches platform profiles.
type ProfileResolver struct {
	hc         *http.Client
	allowLocal bool
	schema     *jsonschema.Schema
	limiter    *rate.Limiter
	sf         singleflight.Group

	mu    sync.Mutex
	cache map[string]profileEntry
}

type profileEntry struct {
	profile *PlatformProfile
	err     *DiscoveryError
	expires time.Time
}

// NewProfileResolver compiles the spec's platform profile schema.
// allowLocal is AllowLocalWebhooks: development and tests serve profiles
// from loopback http.
func NewProfileResolver(allowLocal bool) (*ProfileResolver, error) {
	schema, err := CompileSpec("profile.json#/$defs/platform_schema")
	if err != nil {
		return nil, fmt.Errorf("ucp: platform profile schema: %w", err)
	}
	return &ProfileResolver{
		hc:         platformClient(allowLocal, fetchTimeout),
		allowLocal: allowLocal,
		schema:     schema,
		limiter:    rate.NewLimiter(fetchRate, fetchBurst),
		cache:      map[string]profileEntry{},
	}, nil
}

// Resolve returns the validated profile at raw. Every failure is a
// *DiscoveryError.
func (r *ProfileResolver) Resolve(
	ctx context.Context, raw string,
) (*PlatformProfile, error) {
	if err := ValidatePlatformURL(raw, r.allowLocal); err != nil {
		return nil, discoveryErr(CodeInvalidProfileURL, "%s", err.Error())
	}
	if p, err, ok := r.cached(raw); ok {
		return p, err
	}
	// One fetch per URL however many requests wait on it, detached from
	// the first caller so its disconnect cannot fail the others; each
	// caller still stops waiting on its own context.
	ch := r.sf.DoChan(raw, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		p, ttl, derr := r.fetch(fetchCtx, raw)
		if derr != nil {
			r.remember(raw, profileEntry{
				err: derr, expires: time.Now().Add(failureBackoff),
			})
			return nil, derr
		}
		r.remember(raw, profileEntry{
			profile: p, expires: time.Now().Add(ttl),
		})
		return p, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*PlatformProfile), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *ProfileResolver) cached(raw string) (*PlatformProfile, error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[raw]
	if !ok || time.Now().After(e.expires) {
		return nil, nil, false
	}
	if e.err != nil {
		return nil, e.err, true
	}
	return e.profile, nil, true
}

func (r *ProfileResolver) remember(raw string, e profileEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.cache[raw]; !present && len(r.cache) >= maxProfiles {
		r.evictLocked()
	}
	r.cache[raw] = e
}

// evictLocked drops an expired entry, else the one expiring soonest.
func (r *ProfileResolver) evictLocked() {
	var victim string
	var soonest time.Time
	now := time.Now()
	for k, e := range r.cache {
		if now.After(e.expires) {
			delete(r.cache, k)
			return
		}
		if victim == "" || e.expires.Before(soonest) {
			victim, soonest = k, e.expires
		}
	}
	delete(r.cache, victim)
}

func (r *ProfileResolver) fetch(
	ctx context.Context, raw string,
) (*PlatformProfile, time.Duration, *DiscoveryError) {
	if err := r.limiter.Wait(ctx); err != nil {
		return nil, 0, discoveryErr(CodeProfileUnreachable,
			"profile discovery is over its rate budget; retry shortly")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, discoveryErr(CodeInvalidProfileURL, "%s", err.Error())
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, discoveryErr(CodeProfileUnreachable,
			"unable to fetch the platform profile: %s", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 3xx included: redirects are never followed.
		return nil, 0, discoveryErr(CodeProfileUnreachable,
			"the platform profile answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBytes+1))
	if err != nil {
		return nil, 0, discoveryErr(CodeProfileUnreachable,
			"reading the platform profile: %s", err.Error())
	}
	if len(body) > maxProfileBytes {
		return nil, 0, discoveryErr(CodeProfileMalformed,
			"the platform profile exceeds %d bytes", maxProfileBytes)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return nil, 0, discoveryErr(CodeProfileMalformed,
			"the platform profile is not valid JSON")
	}
	if err := r.schema.Validate(doc); err != nil {
		var verr *jsonschema.ValidationError
		msg := err.Error()
		if errors.As(err, &verr) {
			msg = firstLine(verr.Error())
		}
		return nil, 0, discoveryErr(CodeProfileMalformed,
			"the platform profile violates the UCP platform schema: %s", msg)
	}
	var p PlatformProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, 0, discoveryErr(CodeProfileMalformed,
			"the platform profile cannot be decoded: %s", err.Error())
	}
	return &p, profileTTL(resp.Header.Get("Cache-Control")), nil
}

// profileTTL honours the origin's max-age within the floor and ceiling;
// no-store and no-cache still get the floor, which the spec mandates.
func profileTTL(cacheControl string) time.Duration {
	ttl := profileTTLFloor
	for directive := range strings.SplitSeq(cacheControl, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
		if !strings.EqualFold(name, "max-age") {
			continue
		}
		if secs, err := strconv.Atoi(strings.Trim(value, `"`)); err == nil {
			ttl = time.Duration(secs) * time.Second
		}
	}
	return min(max(ttl, profileTTLFloor), profileTTLCeiling)
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}

// CheckVersion applies request-time version validation: this business
// supports exactly Version and publishes no supported_versions.
func CheckVersion(p *PlatformProfile) error {
	if p.UCP.Version != Version {
		return discoveryErr(CodeVersionUnsupported,
			"protocol version %s is not supported; this business "+
				"supports %s", p.UCP.Version, Version)
	}
	return nil
}
