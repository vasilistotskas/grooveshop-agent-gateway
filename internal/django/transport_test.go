package django

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
			_, _ = w.Write(fixture(t, "tenant_resolve_demostore.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
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

// A budget spent while waiting to retry answers with the failure that
// made the retry, not with the bare deadline.
func TestDeadlineInTheBackoffKeepsTheFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	defer srv.Close()
	p := newProbe(t, srv, 5*time.Second)
	// Shorter than the first backoff (at least 100ms): the deadline
	// passes during the wait, or during the first call.
	ctx, cancel := context.WithTimeout(context.Background(),
		50*time.Millisecond)
	defer cancel()

	err := p.call(ctx, http.MethodGet)

	require.ErrorIs(t, err, ErrUpstreamDown)
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

// A body cut short is the same failure as a connection lost before the
// headers: ErrUpstreamDown, logged, a GET retried unless it timed out.
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
				var hits atomic.Int32
				gotHeaders := make(chan struct{}, 1)
				srv := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, _ *http.Request) {
						if hits.Add(1) > 2 {
							_, _ = w.Write([]byte(`{}`))
							return
						}
						cutBody(t, w, cut.rst, gotHeaders)
					}))
				defer srv.Close()
				p := newProbe(t, srv, 5*time.Second)
				p.afterHeaders(func(*http.Response) { signal(gotHeaders) })

				err := p.call(context.Background(), method)

				assert.Contains(t, p.logs.String(), "django request failed")
				if method == http.MethodGet {
					require.NoError(t, err)
					assert.Equal(t, int32(3), hits.Load(), "retried")
					return
				}
				require.ErrorIs(t, err, ErrUpstreamDown)
				if cut.want != nil {
					assert.ErrorIs(t, err, cut.want)
				} else {
					netErr, ok := errors.AsType[net.Error](err)
					assert.True(t, ok && !netErr.Timeout(), "a reset: %v", err)
				}
				assert.Equal(t, int32(1), hits.Load())
			})
		}
	}
}

// cutBody sends the headers and part of the body, waits until the client
// has the headers, then closes the connection: with a FIN, or with an RST
// when rst is set.
func cutBody(t *testing.T, w http.ResponseWriter, rst bool,
	gotHeaders <-chan struct{}) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if !assert.NoError(t, err) {
		return
	}
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\n"+
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n"+
		`{"cart":`)
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
			p := newProbe(t, srv, 300*time.Millisecond)

			err := p.call(context.Background(), method)

			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Contains(t, p.logs.String(), "django request failed")
			total := 0.0
			for _, n := range p.counts(t) {
				total += n
			}
			assert.InDelta(t, 1, total, 0, "one attempt")
		})
	}
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
