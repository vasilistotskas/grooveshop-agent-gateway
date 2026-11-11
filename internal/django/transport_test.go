package django

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// probe is a client with its log and metrics in reach, against srv.
type probe struct {
	c       *Client
	logs    *bytes.Buffer
	metrics *obs.Metrics
}

func newProbe(t *testing.T, srv *httptest.Server, timeout time.Duration) probe {
	t.Helper()
	logs := &bytes.Buffer{}
	metrics := obs.NewMetrics()
	c := New(srv.URL+"/api/v1", "api.example.test", "test-secret", timeout,
		slog.New(slog.NewTextHandler(logs, nil)), metrics)
	return probe{c: c, logs: logs, metrics: metrics}
}

// afterHeaders runs fn once the response headers have reached the
// client, before do reads the body.
func (p probe) afterHeaders(fn func(*http.Response)) {
	inner := p.c.hc.Transport
	p.c.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(r)
		if resp != nil {
			fn(resp)
		}
		return resp, err
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// call makes one /cart call: get for GET, send for any other method.
func (p probe) call(ctx context.Context, method string) error {
	var out map[string]any
	req := request{path: "/cart", host: "shop.example.test", out: &out}
	if method == http.MethodGet {
		return p.c.get(ctx, req)
	}
	req.body = map[string]string{"a": "b"}
	return p.c.send(ctx, method, req)
}

// counts is gateway_upstream_requests_total by "endpoint method code".
func (p probe) counts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := p.metrics.Registry.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "gateway_upstream_requests_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["endpoint"]+" "+labels["method"]+" "+
				labels["code"]] = metric.GetCounter().GetValue()
		}
	}
	return out
}

var bothMethods = []string{http.MethodGet, http.MethodPost}

// blockUntilGone holds a request open until the client leaves it.
func blockUntilGone(r *http.Request, release <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-release:
	}
}

func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func TestRedirectIsAnError(t *testing.T) {
	statuses := []int{
		http.StatusMultipleChoices, http.StatusMovedPermanently,
		http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
	for _, method := range bothMethods {
		for _, status := range statuses {
			t.Run(method+"/"+http.StatusText(status), func(t *testing.T) {
				var calls, followed atomic.Int32
				mux := http.NewServeMux()
				mux.HandleFunc("/api/v1/cart", func(w http.ResponseWriter,
					r *http.Request) {
					calls.Add(1)
					w.Header().Set("Location", "http://user:pw@"+r.Host+
						"/elsewhere?token=t#frag")
					w.WriteHeader(status)
				})
				mux.HandleFunc("/elsewhere", func(w http.ResponseWriter,
					_ *http.Request) {
					followed.Add(1)
					_, _ = w.Write([]byte(`{}`))
				})
				srv := httptest.NewServer(mux)
				defer srv.Close()
				p := newProbe(t, srv, 2*time.Second)

				err := p.call(context.Background(), method)

				require.ErrorIs(t, err, ErrUpstreamDown)
				assert.Equal(t, int32(1), calls.Load(), "not retried")
				assert.Zero(t, followed.Load(), "not followed")
				assert.Contains(t, p.logs.String(), "location=http://"+
					srv.Listener.Addr().String()+"/elsewhere\n")
				assert.NotContains(t, p.logs.String(), "token")
				assert.NotContains(t, p.logs.String(), "pw")
				assert.NotContains(t, p.logs.String(), "frag")
			})
		}
	}
}

// A redirect's body is read to its end before the call returns, so its
// connection serves the next call. net/http drains a body closed early
// only in the background and for at most 50ms (maxPostCloseReadTime):
// this body's tail comes later than that.
func TestRedirectKeepsTheConnection(t *testing.T) {
	var conns atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/cart", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.Header().Set("Content-Length", "2048")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		_ = http.NewResponseController(w).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
	})
	mux.HandleFunc("/api/v1/ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	p := newProbe(t, srv, 2*time.Second)

	require.ErrorIs(t, p.call(context.Background(), http.MethodGet),
		ErrUpstreamDown)
	require.NoError(t, p.c.get(context.Background(),
		request{path: "/ok", host: "shop.example.test"}))
	assert.Equal(t, int32(1), conns.Load())
}

func TestTimeoutIsNotRetried(t *testing.T) {
	for _, method := range bothMethods {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(
				func(_ http.ResponseWriter, r *http.Request) {
					blockUntilGone(r, release)
				}))
			defer srv.Close()
			defer close(release)
			p := newProbe(t, srv, 200*time.Millisecond)

			err := p.call(context.Background(), method)

			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, p.logs.String(), "django request failed")
			assert.Equal(t, map[string]float64{
				"/cart " + method + " error": 1,
			}, p.counts(t), "one attempt")
		})
	}
}

func TestGetRetriesDroppedConnection(t *testing.T) {
	resolved := fixture(t, "tenant_resolve_demostore.json")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 3 {
				conn, _, err := http.NewResponseController(w).Hijack()
				if !assert.NoError(t, err) {
					return
				}
				_ = conn.Close()
				return
			}
			_, _ = w.Write(resolved)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

// A caller that hangs up (a cancel, a shutdown) is not an upstream
// failure: not retried, not logged, counted as canceled.
func TestCallerHangingUpIsNotAnUpstreamFailure(t *testing.T) {
	cancels := map[string]func() (context.Context, func()){
		"cancel": func() (context.Context, func()) {
			return context.WithCancel(context.Background())
		},
		"shutdown": func() (context.Context, func()) {
			ctx, cancel := context.WithCancelCause(context.Background())
			return ctx, func() { cancel(httpmw.ErrShuttingDown) }
		},
	}
	for _, method := range bothMethods {
		for name, newCtx := range cancels {
			t.Run(method+"/"+name, func(t *testing.T) {
				var hits atomic.Int32
				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(
					func(_ http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						signal(entered)
						blockUntilGone(r, release)
					}))
				defer srv.Close()
				defer close(release)
				p := newProbe(t, srv, 5*time.Second)
				ctx, cancel := newCtx()
				defer cancel()
				go func() {
					<-entered
					cancel()
				}()

				err := p.call(ctx, method)

				require.ErrorIs(t, err, context.Canceled)
				assert.NotErrorIs(t, err, ErrUpstreamDown)
				assert.Equal(t, int32(1), hits.Load())
				assert.Empty(t, p.logs.String())
				assert.Equal(t, map[string]float64{
					"/cart " + method + " canceled": 1,
				}, p.counts(t))
			})
		}
	}
}

func TestCallerHangingUpDuringTheBody(t *testing.T) {
	for _, method := range bothMethods {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte(`{"cart":`))
					_ = http.NewResponseController(w).Flush()
					blockUntilGone(r, release)
				}))
			defer srv.Close()
			defer close(release)
			p := newProbe(t, srv, 5*time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.afterHeaders(func(*http.Response) { cancel() })

			err := p.call(ctx, method)

			require.ErrorIs(t, err, context.Canceled)
			assert.NotErrorIs(t, err, ErrUpstreamDown)
			assert.Empty(t, p.logs.String())
			assert.Equal(t, map[string]float64{
				"/cart " + method + " canceled": 1,
			}, p.counts(t))
		})
	}
}

// A deadline is the gateway's own budget running out while Django had
// not answered: an upstream failure, logged, not retried.
func TestCallerDeadlineIsAnUpstreamFailure(t *testing.T) {
	for _, method := range bothMethods {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(
				func(_ http.ResponseWriter, r *http.Request) {
					blockUntilGone(r, release)
				}))
			defer srv.Close()
			defer close(release)
			p := newProbe(t, srv, 5*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(),
				200*time.Millisecond)
			defer cancel()

			err := p.call(ctx, method)

			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, p.logs.String(), "django request failed")
			assert.Equal(t, map[string]float64{
				"/cart " + method + " error": 1,
			}, p.counts(t))
		})
	}
}

// A context that ends while get waits to retry ends the call: a spent
// budget with the failure the retry was for, a hang-up as callerGone.
func TestWaitToRetry(t *testing.T) {
	req := request{method: http.MethodGet, path: "/cart"}
	failure := &APIError{Status: http.StatusServiceUnavailable}

	expired, cancel := context.WithDeadline(context.Background(),
		time.Now().Add(-time.Second))
	defer cancel()
	assert.Same(t, failure, waitToRetry(expired, time.Hour, req, failure))

	hungUp, hangUp := context.WithCancel(context.Background())
	hangUp()
	err := waitToRetry(hungUp, time.Hour, req, failure)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "django: GET /cart: context canceled", err.Error())

	assert.NoError(t, waitToRetry(context.Background(), 0, req, failure))
}

func TestNoRetryOnceTheCallerHungUp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	defer srv.Close()
	p := newProbe(t, srv, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.afterHeaders(func(*http.Response) { cancel() })

	err := p.call(ctx, http.MethodGet)

	apiErr, ok := errors.AsType[*APIError](err)
	require.True(t, ok, "the 503 that arrived, got %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Status)
	assert.Equal(t, int32(1), hits.Load())
}

// cutServer answers its first cuts requests with status and a body cut
// short, sent once the client has the headers (afterHeaders), and every
// later one with ok. close returns once every cut handler is done.
type cutServer struct {
	srv        *httptest.Server
	hits       atomic.Int32
	handlers   sync.WaitGroup
	gotHeaders chan struct{}
}

func newCutServer(
	t *testing.T, status int, cuts int32, rst bool, part string, ok []byte,
) *cutServer {
	t.Helper()
	s := &cutServer{gotHeaders: make(chan struct{}, 1)}
	s.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if s.hits.Add(1) > cuts {
				_, _ = w.Write(ok)
				return
			}
			s.handlers.Add(1)
			defer s.handlers.Done()
			cutBody(t, w, status, rst, part, s.gotHeaders)
		}))
	return s
}

// probe is a client for the server that tells it when it has headers.
func (s *cutServer) probe(t *testing.T) probe {
	p := newProbe(t, s.srv, 5*time.Second)
	p.afterHeaders(func(*http.Response) { signal(s.gotHeaders) })
	return p
}

func (s *cutServer) close() {
	s.handlers.Wait()
	s.srv.Close()
}

// cutBody sends the headers and part, a body declared longer, waits
// until the client has the headers, then closes the connection: with a
// FIN, or with an RST when rst is set.
func cutBody(t *testing.T, w http.ResponseWriter, status int, rst bool,
	part string, gotHeaders <-chan struct{}) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if !assert.NoError(t, err) {
		return
	}
	defer func() { _ = conn.Close() }()
	_, err = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n"+
		"Location: /elsewhere\r\n"+
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n%s",
		status, http.StatusText(status), part)
	if !assert.NoError(t, err) {
		return
	}
	select {
	case <-gotHeaders:
	case <-time.After(5 * time.Second):
		assert.Fail(t, "the client never read the headers")
	}
	if rst {
		tcp, ok := conn.(*net.TCPConn)
		if assert.True(t, ok) {
			assert.NoError(t, tcp.SetLinger(0))
		}
	}
}

// A body cut short is the same failure as a connection lost before the
// headers: ErrUpstreamDown, logged, counted as error, a GET retried.
func TestBodyCutShort(t *testing.T) {
	cuts := []struct {
		name string
		rst  bool
		want error
	}{
		{"fin", false, io.ErrUnexpectedEOF},
		{"rst", true, nil},
	}
	for _, cut := range cuts {
		for _, method := range bothMethods {
			t.Run(cut.name+"/"+method, func(t *testing.T) {
				s := newCutServer(t, http.StatusOK, 2, cut.rst, `{"cart":`, []byte(`{}`))
				defer s.close()
				p := s.probe(t)

				err := p.call(context.Background(), method)
				s.handlers.Wait()

				assert.Contains(t, p.logs.String(), "django request failed")
				if method == http.MethodGet {
					require.NoError(t, err)
					assert.Equal(t, int32(3), s.hits.Load(), "retried")
					assert.Equal(t, map[string]float64{
						"/cart GET error": 2, "/cart GET 200": 1,
					}, p.counts(t))
					return
				}
				require.ErrorIs(t, err, ErrUpstreamDown)
				if cut.want != nil {
					assert.ErrorIs(t, err, cut.want)
				} else {
					netErr, ok := errors.AsType[net.Error](err)
					assert.True(t, ok && !netErr.Timeout(), "a reset: %v", err)
				}
				assert.Equal(t, int32(1), s.hits.Load())
				assert.Equal(t, map[string]float64{
					"/cart " + method + " error": 1,
				}, p.counts(t))
			})
		}
	}
}

func TestBodyCutByTheClientTimeout(t *testing.T) {
	for _, method := range bothMethods {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte(`{"cart":`))
					_ = http.NewResponseController(w).Flush()
					blockUntilGone(r, release)
				}))
			defer srv.Close()
			defer close(release)
			p := newProbe(t, srv, time.Second)
			var gotHeaders atomic.Bool
			p.afterHeaders(func(*http.Response) { gotHeaders.Store(true) })

			err := p.call(context.Background(), method)

			assert.True(t, gotHeaders.Load(), "timed out in the body")
			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, p.logs.String(), "django request failed")
			assert.Equal(t, map[string]float64{
				"/cart " + method + " error": 1,
			}, p.counts(t), "one attempt")
		})
	}
}

// The status is the whole answer of a call that reads no body.
func TestCutBodyWithoutOutputIsTheStatus(t *testing.T) {
	s := newCutServer(t, http.StatusOK, 1, false, `{"cart":`, nil)
	defer s.close()
	p := s.probe(t)

	err := p.c.send(context.Background(), http.MethodDelete,
		request{path: "/cart", host: "shop.example.test"})

	require.NoError(t, err)
	assert.Equal(t, int32(1), s.hits.Load())
	assert.Empty(t, p.logs.String())
	assert.Equal(t, map[string]float64{"/cart DELETE 200": 1}, p.counts(t))
}

// A whole document is the answer, whatever happens to the connection
// after it.
func TestDocumentThenCutIsTheAnswer(t *testing.T) {
	s := newCutServer(t, http.StatusOK, 1, false, `{"cart":1}`, nil)
	defer s.close()
	p := s.probe(t)

	err := p.call(context.Background(), http.MethodGet)

	require.NoError(t, err)
	assert.Equal(t, int32(1), s.hits.Load())
	assert.Equal(t, map[string]float64{"/cart GET 200": 1}, p.counts(t))
}

// A complete body that is not the JSON expected is not an outage.
func TestInvalidBodyIsNotAnOutage(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"cart":`))
		}))
	defer srv.Close()
	p := newProbe(t, srv, 5*time.Second)

	err := p.call(context.Background(), http.MethodGet)

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUpstreamDown)
	assert.True(t, strings.HasPrefix(err.Error(), "django: decode /cart"))
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, map[string]float64{"/cart GET 200": 1}, p.counts(t))
}

// A transport error names the route's template, like the metric label.
func TestTransportErrorNamesTheRoute(t *testing.T) {
	logs := &bytes.Buffer{}
	c := New("http://127.0.0.1:1/api/v1", "api.example.test", "s",
		time.Second, slog.New(slog.NewTextHandler(logs, nil)), nil)

	err := c.send(context.Background(), http.MethodGet, request{
		path:    "/order/uuid/{uuid}",
		rawPath: "/order/uuid/0b1c2d3e",
		query:   url.Values{"q": {"typed"}},
		host:    "shop.example.test",
	})

	require.ErrorIs(t, err, ErrUpstreamDown)
	route := "http://127.0.0.1:1/api/v1/order/uuid/{uuid}"
	assert.Contains(t, err.Error(), route)
	assert.Contains(t, logs.String(), route)
	for _, s := range []string{err.Error(), logs.String()} {
		assert.NotContains(t, s, "0b1c2d3e")
		assert.NotContains(t, s, "typed")
	}
}

// GET /cart naming no cart creates one in Django: a retry would create
// another, so it is sent once. A named cart, by id or by the shopper's
// token, is only read: retried.
func TestGetCartNamingNoCartIsSentOnce(t *testing.T) {
	cart := fixture(t, "cart_with_items.json")
	tok, err := ParseCartToken(testCartToken)
	require.NoError(t, err)
	cases := []struct {
		name   string
		cartID string
		ctx    context.Context
		hits   int32
	}{
		{"new", "", context.Background(), 1},
		{"named", fixtureCartID, context.Background(), 3},
		{"token", "", WithCartToken(context.Background(), tok), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCutServer(t, http.StatusOK, 2, false, `{"id":`, cart)
			defer s.close()
			p := s.probe(t)

			_, err := p.c.GetCart(tc.ctx,
				"shop.example.test", "el", tc.cartID)
			s.handlers.Wait()

			if tc.hits == 1 {
				require.ErrorIs(t, err, ErrUpstreamDown)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.hits, s.hits.Load())
		})
	}
}

func TestPingIsOneAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			assert.Equal(t, "/api/v1/health/live", r.URL.Path)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.ErrorIs(t, c.Ping(context.Background()), ErrUpstreamDown)
	assert.Equal(t, int32(1), calls.Load())
}

// An error body or a redirect body is read like any other: a failed
// read is an outage, or the caller's hang-up, whatever the status.
func TestErrorBodyCutShort(t *testing.T) {
	cuts := []struct {
		name string
		rst  bool
	}{{"fin", false}, {"rst", true}}
	for _, cut := range cuts {
		for _, method := range bothMethods {
			t.Run(cut.name+"/"+method, func(t *testing.T) {
				s := newCutServer(t, http.StatusServiceUnavailable, 3,
					cut.rst, `{"detail":`, nil)
				defer s.close()
				p := s.probe(t)

				err := p.call(context.Background(), method)
				s.handlers.Wait()

				require.ErrorIs(t, err, ErrUpstreamDown)
				_, isAPIErr := errors.AsType[*APIError](err)
				assert.False(t, isAPIErr, "not a 503: %v", err)
				assert.Contains(t, p.logs.String(), "django request failed")
				attempts := 1
				if method == http.MethodGet {
					attempts = 3
				}
				assert.Equal(t, int32(attempts), s.hits.Load())
				assert.Equal(t, map[string]float64{
					"/cart " + method + " error": float64(attempts),
				}, p.counts(t))
			})
		}
	}
}

// slowBody answers status with part of a body declared longer, then
// holds the rest until the client leaves.
func slowBody(status int, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":`))
		_ = http.NewResponseController(w).Flush()
		blockUntilGone(r, release)
	}
}

func TestErrorBodyCutByTheClientTimeout(t *testing.T) {
	for _, method := range bothMethods {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(slowBody(
				http.StatusServiceUnavailable, release))
			defer srv.Close()
			defer close(release)
			p := newProbe(t, srv, time.Second)
			var gotHeaders atomic.Bool
			p.afterHeaders(func(*http.Response) { gotHeaders.Store(true) })

			err := p.call(context.Background(), method)

			assert.True(t, gotHeaders.Load(), "timed out in the body")
			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Equal(t, map[string]float64{
				"/cart " + method + " error": 1,
			}, p.counts(t), "one attempt")
		})
	}
}

func TestCallerHangingUpDuringAnErrorOrRedirectBody(t *testing.T) {
	for _, status := range []int{
		http.StatusServiceUnavailable, http.StatusFound,
	} {
		for _, method := range bothMethods {
			t.Run(strconv.Itoa(status)+"/"+method, func(t *testing.T) {
				release := make(chan struct{})
				srv := httptest.NewServer(slowBody(status, release))
				defer srv.Close()
				defer close(release)
				p := newProbe(t, srv, 5*time.Second)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p.afterHeaders(func(*http.Response) { cancel() })

				err := p.call(ctx, method)

				require.ErrorIs(t, err, context.Canceled)
				assert.NotErrorIs(t, err, ErrUpstreamDown)
				assert.Empty(t, p.logs.String())
				assert.Equal(t, map[string]float64{
					"/cart " + method + " canceled": 1,
				}, p.counts(t))
			})
		}
	}
}

// A redirect whose body is cut by anything but the caller is still the
// redirect: it was received.
func TestRedirectBodyCutIsStillARedirect(t *testing.T) {
	s := newCutServer(t, http.StatusFound, 1, false, `moved`, nil)
	defer s.close()
	p := s.probe(t)

	err := p.call(context.Background(), http.MethodGet)

	require.ErrorIs(t, err, ErrUpstreamDown)
	assert.Contains(t, err.Error(), "redirected with status 302")
	assert.Contains(t, p.logs.String(), "django redirected a call")
	assert.NotContains(t, p.logs.String(), "django request failed")
	assert.Equal(t, int32(1), s.hits.Load())
	assert.Equal(t, map[string]float64{"/cart GET 302": 1}, p.counts(t))
}
