package feeds

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
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

// memRedis keeps keys in a map behind scriptedRedis: MGET, TOUCH,
// UNLINK, GET and SET act on it (a missing key reads as redis.Nil, as
// from Redis), and every other command succeeds.
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
		var n int64
		for _, k := range args[1:] {
			if _, ok := m.vals[fmt.Sprint(k)]; ok {
				n++
				if c.Name() == "unlink" {
					delete(m.vals, fmt.Sprint(k))
				}
			}
		}
		c.SetVal(n)
	case *redis.Cmd:
		// dropJudged: EVALSHA sha 2 blob meta record.
		if c.Name() == "evalsha" && m.vals[fmt.Sprint(args[4])] == args[5] {
			delete(m.vals, fmt.Sprint(args[3]))
		}
		c.SetVal(int64(0))
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

const brokenLine = "cached feed blob is broken"

// A blob that is not this feed's whole gzip (checked from its header and
// trailer, without decoding) is never served: it is dropped and a miss,
// regenerated like an evicted one, and while a failed generation's
// backoff runs it answers 503 at once, uncacheable, with the cause logged
// once under the request.
func TestFeedHandlerNeverServesABrokenBlob(t *testing.T) {
	gz := gzipped(t, testFeed)
	// The trailer's CRC-32 and ISIZE, zeroed.
	badTrailer := append(bytes.Clone(gz[:len(gz)-8]), make([]byte, 8)...)
	for name, entry := range map[string]map[string]string{
		"not gzip": cachedRSS(t, []byte("not gzip"), len(testFeed)),
		"empty":    cachedRSS(t, nil, len(testFeed)),
		// Long enough, with a true ISIZE, but not a gzip header.
		"not gzip, size in its tail": cachedRSS(t,
			binary.LittleEndian.AppendUint32(make([]byte, 16),
				uint32(len(testFeed))), len(testFeed)),
		// A gzip header and a true ISIZE, but no room for a member.
		"header and size only": cachedRSS(t,
			binary.LittleEndian.AppendUint32([]byte{0x1f, 0x8b, 8},
				uint32(len(testFeed))), len(testFeed)),
		"cut short":        cachedRSS(t, gz[:len(gz)-6], len(testFeed)),
		"bad gzip trailer": cachedRSS(t, badTrailer, len(testFeed)),
		"another feed": cachedRSS(t, gzipped(t, testFeed+"<!-- -->"),
			len(testFeed)),
		"size too large": cachedRSS(t, gz, len(testFeed)+1),
		"size too small": cachedRSS(t, gz, len(testFeed)-1),
	} {
		t.Run(name+"/backing off", func(t *testing.T) {
			for _, accept := range []string{"gzip", "identity"} {
				entry := maps.Clone(entry)
				entry[failedKey("demostore")] = "1"
				m, rdb := newMemRedis(entry)
				var logs bytes.Buffer
				resp := serveFeed(t, feedService(nil, rdb, &logs),
					http.MethodGet, map[string]string{"Accept-Encoding": accept})
				body := readAll(t, resp)
				require.Equal(t, http.StatusServiceUnavailable,
					resp.StatusCode, accept)
				uncacheable(t, resp)
				assert.NotContains(t, string(body), "<rss>", accept)
				lines := logLines(t, &logs, brokenLine)
				require.Len(t, lines, 1, accept)
				assert.Equal(t, "ERROR", lines[0]["level"])
				assert.Equal(t, "req-131", lines[0][obs.KeyRequestID])
				_, kept := m.get(dataKey("demostore", formatRSS))
				assert.False(t, kept, "the broken blob is dropped")
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

// brokenMiddle is a gzip member with a whole header and a trailer true
// to raw (CRC-32, ISIZE), whose deflate data holds the first half of raw
// in a stored block and then a block of the reserved type, which fails
// to decode (RFC 1951 3.2.3).
func brokenMiddle(raw string) []byte {
	half := raw[:len(raw)/2]
	b := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}
	b = append(b, 0) // BFINAL 0, BTYPE 00 (stored)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(half)))
	b = binary.LittleEndian.AppendUint16(b, ^uint16(len(half)))
	b = append(b, half...)
	b = append(b, 0b111) // BFINAL 1, BTYPE 11 (reserved)
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE([]byte(raw)))
	return binary.LittleEndian.AppendUint32(b, uint32(len(raw)))
}

// longerThanItsSize is the gzip of raw with more after it, whose ISIZE
// is raw's length: whole to look at, longer once decoded.
func longerThanItsSize(t *testing.T, raw, more string) []byte {
	t.Helper()
	gz := gzipped(t, raw+more)
	return binary.LittleEndian.AppendUint32(gz[:len(gz)-4:len(gz)-4],
		uint32(len(raw)))
}

// A blob whose header and trailer are whole but that does not decode to
// exactly its size, CRC included, is found only while the identity body
// is streamed. The end of the body is held back until gzip has verified
// it, so the response always ends short of its declared length, which
// net/http answers by closing the connection: the client sees an error,
// never a complete 200. The cause is logged once and the blob dropped,
// so the next request regenerates it.
func TestFeedHandlerCutsAnIdentityBodyThatFailsToDecode(t *testing.T) {
	for _, raw := range []string{testFeed, bigFeed(3*holdBack + 7)} {
		for name, blob := range map[string][]byte{
			"broken middle":        brokenMiddle(raw),
			"longer than its size": longerThanItsSize(t, raw, "<!-- -->"),
			// More than the hold-back past it: decoding stops at the
			// declared length, never writing past it.
			"far longer than its size": longerThanItsSize(t, raw,
				bigFeed(3*holdBack)),
			// Same length, CRC-32 flipped: only gzip's check at the
			// end finds it, after the whole body has been decoded.
			"bad CRC": badCRC(t, raw),
		} {
			t.Run(fmt.Sprintf("%s/%d bytes", name, len(raw)),
				func(t *testing.T) { cutsAnIdentityBody(t, raw, blob) })
		}
	}
}

// bigFeed is n bytes of feed-like text.
func bigFeed(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "<item><g:id>%d</g:id></item>", i)
	}
	return b.String()[:n]
}

// badCRC is the gzip of raw with its CRC-32 flipped.
func badCRC(t *testing.T, raw string) []byte {
	t.Helper()
	gz := gzipped(t, raw)
	gz[len(gz)-8] ^= 0xff
	return gz
}

func cutsAnIdentityBody(t *testing.T, raw string, blob []byte) {
	m, rdb := newMemRedis(cachedRSS(t, blob, len(raw)))
	var logs bytes.Buffer
	s := feedService(oneProductCatalog(t), rdb, &logs)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.NewContext(obs.WithRequestID(r.Context(),
				"req-131"), demoTenant())
			s.Handler(KindGoogle).ServeHTTP(w, r.WithContext(ctx))
		}))
	t.Cleanup(srv.Close)
	get := func() (*http.Response, []byte, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := http.DefaultTransport.RoundTrip(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body, err
	}

	resp, body, err := get()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, strconv.Itoa(len(raw)),
		resp.Header.Get("Content-Length"))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Less(t, len(body), len(raw), "never the declared length")
	assert.True(t, strings.HasPrefix(raw, string(body)))
	lines := logLines(t, &logs, brokenLine)
	require.Len(t, lines, 1)
	assert.Equal(t, "req-131", lines[0][obs.KeyRequestID])
	_, kept := m.get(dataKey("demostore", formatRSS))
	assert.False(t, kept, "the broken blob is dropped")

	resp, body, err = get()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, strconv.Itoa(len(body)),
		resp.Header.Get("Content-Length"))
	assert.True(t, strings.HasPrefix(string(body), "<?xml"))
}

// A healthy feed goes out byte for byte whether it is shorter than the
// hold-back, as long, or longer.
func TestFeedHandlerStreamsAWholeIdentityBody(t *testing.T) {
	for _, n := range []int{
		0, 1, holdBack - 1, holdBack, holdBack + 1, 2 * holdBack,
		3*holdBack + 7,
	} {
		raw := bigFeed(n)
		_, rdb := newMemRedis(cachedRSS(t, gzipped(t, raw), n))
		var logs bytes.Buffer
		resp := serveFeed(t, feedService(nil, rdb, &logs), http.MethodGet,
			map[string]string{"Accept-Encoding": "identity"})
		body := readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, n)
		assert.Equal(t, strconv.Itoa(n), resp.Header.Get("Content-Length"), n)
		assert.Equal(t, raw, string(body), n)
		assert.Empty(t, logs.String(), n)
	}
}

// The gzip coding writes the blob as stored and decodes nothing, and a
// HEAD decodes nothing in either coding: a blob whose middle does not
// decode goes out byte for byte, nothing is logged, and it is kept.
func TestFeedHandlerDecodesOnlyAnIdentityGET(t *testing.T) {
	blob := brokenMiddle(testFeed)
	for _, tc := range []struct {
		method, accept string
		body           []byte
	}{
		{http.MethodGet, "gzip", blob},
		{http.MethodHead, "gzip", []byte{}},
		{http.MethodHead, "identity", []byte{}},
	} {
		name := tc.method + " " + tc.accept
		m, rdb := newMemRedis(cachedRSS(t, blob, len(testFeed)))
		var logs bytes.Buffer
		resp := serveFeed(t, feedService(nil, rdb, &logs), tc.method,
			map[string]string{"Accept-Encoding": tc.accept})
		body := readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Equal(t, tc.body, body, name)
		assert.Empty(t, logs.String(), name)
		_, kept := m.get(dataKey("demostore", formatRSS))
		assert.True(t, kept, name)
	}
}

// dropBlob removes the blob only while the stored record is the one it
// was judged with (memRedis stands in for the script; the integration
// suite runs it on Redis).
func TestDropBlobSparesAReplacement(t *testing.T) {
	judged := cachedRSS(t, []byte("not gzip"), len(testFeed))
	var meta Meta
	raw := judged[metaKey("demostore", formatRSS)]
	require.NoError(t, json.Unmarshal([]byte(raw), &meta))
	meta.record = raw
	// The generation that replaced it, a second later.
	newer := meta
	newer.GeneratedAt = newer.GeneratedAt.Add(time.Second)
	newerRaw, err := json.Marshal(newer)
	require.NoError(t, err)

	m, rdb := newMemRedis(map[string]string{
		dataKey("demostore", formatRSS): string(gzipped(t, testFeed)),
		metaKey("demostore", formatRSS): string(newerRaw),
	})
	feedService(nil, rdb, io.Discard).dropBlob(context.Background(),
		"demostore", formatRSS, meta, errBroken)
	_, kept := m.get(dataKey("demostore", formatRSS))
	assert.True(t, kept, "a blob written since the judgement stays")

	m, rdb = newMemRedis(judged)
	feedService(nil, rdb, io.Discard).dropBlob(context.Background(),
		"demostore", formatRSS, meta, errBroken)
	_, kept = m.get(dataKey("demostore", formatRSS))
	assert.False(t, kept, "the blob judged goes")
}
