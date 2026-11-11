package feeds

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func TestServesGzip(t *testing.T) {
	for _, tc := range []struct {
		header []string
		want   bool
	}{
		{header: nil, want: false},
		{header: []string{""}, want: false},
		{header: []string{"gzip"}, want: true},
		{header: []string{"gzip, deflate, br"}, want: true},
		// Unlisted identity has no weight to beat gzip's.
		{header: []string{"gzip;q=0.5"}, want: true},
		{header: []string{"gzip;q=0.001, br"}, want: true},
		{header: []string{"gzip;q=0.5, identity;q=0.8"}, want: false},
		{header: []string{"gzip;q=0.5, identity;q=0.5"}, want: true},
		{header: []string{"gzip;q=0.5, *;q=0.8"}, want: false},
		{header: []string{"*"}, want: true},
		{header: []string{"gzip;q=0, br"}, want: false},
		{header: []string{"br"}, want: false},
		// Neither acceptable: identity, never a refusal.
		{header: []string{"br, identity;q=0, *;q=0"}, want: false},
		{header: []string{"br", "gzip"}, want: true},
	} {
		assert.Equal(t, tc.want, servesGzip(tc.header), "%q", tc.header)
	}
}

// memRedis keeps keys in a map behind scriptedRedis: MGET, TOUCH, GET
// and SET act on it (a missing key reads as redis.Nil, as from Redis),
// and every other command succeeds.
type memRedis struct {
	scriptedRedis
	mu   sync.Mutex
	vals map[string]string
}

func newMemRedis(vals map[string]string) (*memRedis, *redis.Client) {
	m := &memRedis{vals: vals}
	m.answer = m.do
	return m, m.client()
}

func (m *memRedis) do(cmd redis.Cmder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	args := cmd.Args()
	switch c := cmd.(type) {
	case *redis.SliceCmd:
		if c.Name() == "mget" {
			vals := make([]any, len(args)-1)
			for i, k := range args[1:] {
				if v, ok := m.vals[fmt.Sprint(k)]; ok {
					vals[i] = v
				}
			}
			c.SetVal(vals)
		}
	case *redis.IntCmd:
		if c.Name() == "touch" {
			var n int64
			for _, k := range args[1:] {
				if _, ok := m.vals[fmt.Sprint(k)]; ok {
					n++
				}
			}
			c.SetVal(n)
		}
	case *redis.StringCmd:
		v, ok := m.vals[fmt.Sprint(args[1])]
		if !ok {
			return redis.Nil
		}
		c.SetVal(v)
	case *redis.StatusCmd:
		if c.Name() == "set" {
			v := args[2]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			m.vals[fmt.Sprint(args[1])] = fmt.Sprint(v)
		}
	}
	return nil
}

func (m *memRedis) get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[key]
	return v, ok
}

const testFeed = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<rss><channel><title>Store</title></channel></rss>`

// cachedRSS is a fresh, cached RSS entry whose blob is gz and whose
// stored size is size.
func cachedRSS(t *testing.T, gz []byte, size int) map[string]string {
	t.Helper()
	meta, err := json.Marshal(Meta{
		ETag: `W/"feed"`, GeneratedAt: time.Now(), Size: size,
		Generator: "test",
	})
	require.NoError(t, err)
	return map[string]string{
		dataKey("demostore", formatRSS): string(gz),
		metaKey("demostore", formatRSS): string(meta),
	}
}

func gzipped(t *testing.T, raw string) []byte {
	t.Helper()
	gz, err := gzipBytes([]byte(raw))
	require.NoError(t, err)
	return gz
}

func feedService(
	dj *django.Client, rdb *redis.Client, logs io.Writer,
) *Service {
	log := slog.New(obs.NewContextHandler(slog.NewJSONHandler(logs, nil)))
	return NewService(dj, rdb, log, obs.NewMetrics(), "", "", time.Hour,
		24*time.Hour, "test", storefront.NewSite("el"))
}

// serveFeed runs one request through the google.xml handler, as the
// request with id requestID, and returns the response as the handler
// wrote it.
func serveFeed(
	t *testing.T, s *Service, method string, header map[string]string,
) *http.Response {
	t.Helper()
	ctx := tenant.NewContext(
		obs.WithRequestID(context.Background(), "req-131"), demoTenant())
	req := httptest.NewRequestWithContext(ctx, method,
		"/feeds/google.xml", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler(KindGoogle).ServeHTTP(rec, req)
	return rec.Result()
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return body
}

// A 200 declares the length of what it sends, in either coding, and a
// HEAD answers the same header fields with no content (RFC 9110 9.3.2,
// 8.6).
func TestFeedHandlerDeclaresWhatItSends(t *testing.T) {
	gz := gzipped(t, testFeed)
	for _, tc := range []struct {
		accept   string
		encoding string
		body     []byte
	}{
		{"gzip", "gzip", gz},
		{"identity", "", []byte(testFeed)},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			name := tc.accept + " " + method
			_, rdb := newMemRedis(cachedRSS(t, gz, len(testFeed)))
			resp := serveFeed(t, feedService(nil, rdb, io.Discard), method,
				map[string]string{"Accept-Encoding": tc.accept})
			body := readAll(t, resp)

			require.Equal(t, http.StatusOK, resp.StatusCode, name)
			assert.Equal(t, `W/"feed"`, resp.Header.Get("ETag"), name)
			assert.Equal(t, "public, max-age=3600",
				resp.Header.Get("Cache-Control"), name)
			assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"), name)
			assert.Equal(t, "application/xml; charset=utf-8",
				resp.Header.Get("Content-Type"), name)
			assert.Equal(t, tc.encoding,
				resp.Header.Get("Content-Encoding"), name)
			assert.Equal(t, strconv.Itoa(len(tc.body)),
				resp.Header.Get("Content-Length"), name)
			if method == http.MethodHead {
				assert.Empty(t, body, name)
				continue
			}
			assert.Equal(t, tc.body, body, name)
		}
	}
}

// A 304 carries the validators and the caching fields a 200 would, and
// no representation metadata (RFC 9110 15.4.5).
func TestFeedHandlerNotModified(t *testing.T) {
	gz := gzipped(t, testFeed)
	for _, accept := range []string{"gzip", "identity"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			name := accept + " " + method
			_, rdb := newMemRedis(cachedRSS(t, gz, len(testFeed)))
			resp := serveFeed(t, feedService(nil, rdb, io.Discard), method,
				map[string]string{
					"Accept-Encoding": accept, "If-None-Match": `W/"feed"`,
				})
			assert.Empty(t, readAll(t, resp), name)

			require.Equal(t, http.StatusNotModified, resp.StatusCode, name)
			assert.Equal(t, `W/"feed"`, resp.Header.Get("ETag"), name)
			assert.Equal(t, "public, max-age=3600",
				resp.Header.Get("Cache-Control"), name)
			assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"), name)
			for _, field := range []string{
				"Content-Type", "Content-Encoding", "Content-Length",
			} {
				assert.Empty(t, resp.Header.Values(field), name+" "+field)
			}
		}
	}
}

// uncacheable asserts an error answer carries none of the feed's
// caching fields.
func uncacheable(t *testing.T, resp *http.Response) {
	t.Helper()
	for _, field := range []string{
		"ETag", "Cache-Control", "Vary", "Content-Encoding",
	} {
		assert.Empty(t, resp.Header.Values(field), field)
	}
}

// logLines returns the lines logged with msg.
func logLines(t *testing.T, logs *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(logs)
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		if line["msg"] == msg {
			lines = append(lines, line)
		}
	}
	return lines
}

// A blob that does not decode to its stored size is never served: it
// is a miss, regenerated like an evicted one, and while a failed
// generation's backoff runs it answers 503 at once, uncacheable, with
// the cause logged once under the request.
func TestFeedHandlerNeverServesABrokenBlob(t *testing.T) {
	gz := gzipped(t, testFeed)
	// The trailer's CRC-32 and size, zeroed: only reading to the end
	// finds it.
	badTrailer := append(bytes.Clone(gz[:len(gz)-8]), make([]byte, 8)...)
	for name, entry := range map[string]map[string]string{
		"not gzip":         cachedRSS(t, []byte("not gzip"), len(testFeed)),
		"cut short":        cachedRSS(t, gz[:len(gz)-6], len(testFeed)),
		"bad gzip trailer": cachedRSS(t, badTrailer, len(testFeed)),
		"size too large":   cachedRSS(t, gz, len(testFeed)+1),
		"size too small":   cachedRSS(t, gz, len(testFeed)-1),
	} {
		t.Run(name+"/backing off", func(t *testing.T) {
			entry := maps.Clone(entry)
			entry[failedKey("demostore")] = "1"
			_, rdb := newMemRedis(entry)
			var logs bytes.Buffer
			for _, accept := range []string{"gzip", "identity"} {
				logs.Reset()
				resp := serveFeed(t, feedService(nil, rdb, &logs),
					http.MethodGet, map[string]string{"Accept-Encoding": accept})
				body := readAll(t, resp)
				require.Equal(t, http.StatusServiceUnavailable,
					resp.StatusCode, accept)
				uncacheable(t, resp)
				assert.NotContains(t, string(body), "<rss>", accept)
				lines := logLines(t, bytes.NewBuffer(logs.Bytes()),
					"cached feed does not decode")
				require.Len(t, lines, 1, accept+": "+logs.String())
				assert.Equal(t, "ERROR", lines[0]["level"])
				assert.Equal(t, "req-131", lines[0][obs.KeyRequestID])
			}
		})
		t.Run(name+"/regenerated", func(t *testing.T) {
			m, rdb := newMemRedis(maps.Clone(entry))
			resp := serveFeed(t,
				feedService(oneProductCatalog(t), rdb, io.Discard),
				http.MethodGet, map[string]string{"Accept-Encoding": "identity"})
			body := readAll(t, resp)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.True(t, strings.HasPrefix(string(body), "<?xml"))
			assert.Equal(t, strconv.Itoa(len(body)),
				resp.Header.Get("Content-Length"))
			stored, ok := m.get(dataKey("demostore", formatRSS))
			require.True(t, ok)
			zr, err := gzip.NewReader(strings.NewReader(stored))
			require.NoError(t, err)
			regenerated, err := io.ReadAll(zr)
			require.NoError(t, err)
			assert.Equal(t, body, regenerated)
		})
	}
}

// A feed that cannot be served answers 503 with none of the feed's
// caching fields.
func TestFeedHandlerUnavailableIsUncacheable(t *testing.T) {
	_, rdb := newMemRedis(map[string]string{failedKey("demostore"): "1"})
	resp := serveFeed(t, feedService(nil, rdb, io.Discard), http.MethodGet,
		map[string]string{"Accept-Encoding": "gzip"})
	readAll(t, resp)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	uncacheable(t, resp)
}

// The gzip and the identity body share one validator, so it is weak
// (RFC 9110 8.8.3.3), and a client revalidating with the tag it was
// given gets 304 in either coding.
func TestGeneratedFeedHasOneWeakValidatorForBothCodings(t *testing.T) {
	_, rdb := newMemRedis(map[string]string{})
	s := feedService(oneProductCatalog(t), rdb, io.Discard)
	tags := map[string]string{}
	for _, accept := range []string{"gzip", "identity"} {
		resp := serveFeed(t, s, http.MethodGet,
			map[string]string{"Accept-Encoding": accept})
		readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		tags[accept] = resp.Header.Get("ETag")
		assert.Regexp(t, `^W/"[0-9a-f]{32}"$`, tags[accept])
	}
	assert.Equal(t, tags["gzip"], tags["identity"])
	for _, given := range tags {
		for _, accept := range []string{"gzip", "identity"} {
			resp := serveFeed(t, s, http.MethodGet, map[string]string{
				"Accept-Encoding": accept, "If-None-Match": given,
			})
			readAll(t, resp)
			assert.Equal(t, http.StatusNotModified, resp.StatusCode, accept)
		}
	}
}
