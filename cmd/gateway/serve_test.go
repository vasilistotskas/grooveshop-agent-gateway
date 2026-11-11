package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// lockedBuffer collects log lines written from serve's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServe runs serve on a loopback port with ctx and returns its
// address, its logs and serve's result.
func startServe(
	t *testing.T, ctx context.Context, handler http.Handler, tm timing,
) (string, *lockedBuffer, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	logs := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, handler, tm,
			slog.New(slog.NewTextHandler(logs, nil)))
	}()
	return "http://" + ln.Addr().String(), logs, done
}

func stoppable(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	return ctx, stop
}

// openStream sends a request with body and returns the response body
// once the handler has flushed the first line.
func openStream(t *testing.T, url, body string) *bufio.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "open\n", line)
	return reader
}

// opened writes and flushes the line openStream waits for.
func opened(w http.ResponseWriter) {
	_, _ = io.WriteString(w, "open\n")
	_ = http.NewResponseController(w).Flush()
}

func waitServe(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		require.FailNow(t, "serve did not return")
		return nil
	}
}

// A stream still open when the drain ends sees its context cancelled
// as a shutdown, writes its final event, and the server stops cleanly
// inside the window.
func TestServeEndsAStreamWithAFinalEvent(t *testing.T) {
	tm := timing{
		read: time.Minute, drain: 100 * time.Millisecond,
		final: 5 * time.Second,
	}
	causes := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opened(w)
		<-r.Context().Done()
		causes <- context.Cause(r.Context())
		_, _ = io.WriteString(w, "final\n")
	})
	ctx, stop := stoppable(t)
	url, logs, done := startServe(t, ctx, handler, tm)
	body := openStream(t, url, "")

	began := time.Now()
	stop()

	require.NoError(t, waitServe(t, done, tm.drain+tm.final))
	assert.GreaterOrEqual(t, time.Since(began), tm.drain,
		"the request kept a live context through the drain")
	assert.ErrorIs(t, <-causes, httpmw.ErrShuttingDown)
	rest, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "final\n", string(rest))
	assert.NotContains(t, logs.String(), "level=WARN")
}

// A stream cut at shutdown still leaves its access line, under its
// request id.
func TestServeLogsTheRequestItCut(t *testing.T) {
	tm := timing{
		read: time.Minute, drain: 50 * time.Millisecond,
		final: 5 * time.Second,
	}
	access := &lockedBuffer{}
	log := slog.New(obs.NewContextHandler(slog.NewTextHandler(access, nil)))
	handler := httpmw.RequestID()(httpmw.Logging(log)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			opened(w)
			<-r.Context().Done()
			_, _ = io.WriteString(w, "final\n")
		})))
	ctx, stop := stoppable(t)
	url, _, done := startServe(t, ctx, handler, tm)
	body := openStream(t, url, "")

	stop()

	require.NoError(t, waitServe(t, done, tm.drain+tm.final))
	_, _ = io.ReadAll(body)
	assert.Contains(t, access.String(), "msg=request")
	assert.Contains(t, access.String(), "status=200")
	assert.Contains(t, access.String(), "request_id=")
}

// A panic in the middleware, outside httpmw.Recover, reaches net/http's
// own recover; its line goes through the gateway's handler, at error.
func TestServeLogsAMiddlewarePanicThroughTheGatewayLogger(t *testing.T) {
	handler := httpmw.RequestID()(httpmw.Logging(slog.New(slog.DiscardHandler))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("middleware boom")
		})))
	ctx, stop := stoppable(t)
	url, logs, done := startServe(t, ctx, handler, podTiming(0))

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "net/http drops the connection")

	assert.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "middleware boom")
	}, 5*time.Second, 10*time.Millisecond)
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), "http: panic serving")
	stop()
	tm := podTiming(0)
	require.NoError(t, waitServe(t, done, tm.drain+tm.final))
}

// A handler that ignores the cancellation is cut when the window ends,
// and that is a warning, not a failed exit.
func TestServeCutsWhatOutlivesTheWindow(t *testing.T) {
	tm := timing{
		read: time.Minute, drain: 50 * time.Millisecond,
		final: 50 * time.Millisecond,
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		opened(w)
		<-release
	})
	ctx, stop := stoppable(t)
	url, logs, done := startServe(t, ctx, handler, tm)
	body := openStream(t, url, "")

	stop()

	require.NoError(t, waitServe(t, done, 5*time.Second))
	_, err := io.ReadAll(body)
	assert.Error(t, err, "the cut stream ends without its chunked end")
	assert.Contains(t, logs.String(), "level=WARN")
}

// With nothing in flight the server stops at once, without waiting for
// the drain.
func TestServeStopsAtOnceWithNothingInFlight(t *testing.T) {
	tm := timing{read: time.Minute, drain: time.Minute, final: time.Minute}
	ctx, stop := stoppable(t)
	_, logs, done := startServe(t, ctx, http.NotFoundHandler(), tm)

	stop()

	require.NoError(t, waitServe(t, done, 5*time.Second))
	assert.NotContains(t, logs.String(), "level=WARN")
}

// A signal that arrived before serve started is a clean stop.
func TestServeWithAContextAlreadyDone(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	stop()

	_, logs, done := startServe(t, ctx, http.NotFoundHandler(), podTiming(0))

	require.NoError(t, waitServe(t, done, 5*time.Second))
	assert.NotContains(t, logs.String(), "level=WARN")
}

// The read timeout bounds reading the request, not the response: a
// stream whose body was read outlives it with its context live.
func TestServeStreamOutlivesTheReadTimeout(t *testing.T) {
	tm := timing{
		read: 100 * time.Millisecond, drain: time.Second, final: time.Second,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		opened(w)
		select {
		case <-time.After(4 * tm.read):
			_, _ = io.WriteString(w, "still here\n")
		case <-r.Context().Done():
			_, _ = io.WriteString(w, "cancelled\n")
		}
	})
	ctx, stop := stoppable(t)
	url, _, done := startServe(t, ctx, handler, tm)
	body := openStream(t, url, `{"message": "hello"}`)

	rest, err := io.ReadAll(body)

	require.NoError(t, err)
	assert.Equal(t, "still here\n", string(rest))
	stop()
	require.NoError(t, waitServe(t, done, 5*time.Second))
}

// A listener that fails is still an error.
func TestServeReturnsAListenerFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	err = serve(context.Background(), ln, http.NotFoundHandler(),
		podTiming(0), slog.New(slog.DiscardHandler))

	assert.Error(t, err)
}

// An order completion can detach at the very end of the drain, so the
// final window holds its whole detached budget, then the key settle and
// the lock release (one upstream timeout each), with room left to write
// the answer; a stream keeps its own window when that is longer.
func TestPodTimingHoldsACompletion(t *testing.T) {
	for _, upstream := range []time.Duration{time.Second, 10 * time.Second} {
		tm := podTiming(completionTail(upstream))

		settled := checkout.CompletionBudget(upstream) + 2*upstream
		assert.Greater(t, tm.final, settled, upstream)
		assert.GreaterOrEqual(t, tm.final, streamFinal, upstream)
	}
}

// An MCP tool runs on a goroutine of the SDK's own. The request that
// carried it stays open until the tool returns, even once its context
// has ended at the cut, so a tool finishing work it detached (an order
// completion) inside the window is not cut by the process exiting.
func TestServeWaitsForAToolThatOutlivesItsRequest(t *testing.T) {
	tm := timing{
		read: time.Minute, drain: 50 * time.Millisecond,
		final: 5 * time.Second,
	}
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "place"}, func(
		context.Context, *mcp.CallToolRequest, struct{},
	) (*mcp.CallToolResult, struct{}, error) {
		close(started)
		<-release // work that runs on without the caller's context
		close(finished)
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	ctx, stop := stoppable(t)
	url, logs, done := startServe(t, ctx, mcpHandler(server), tm)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(context.Background(),
			&mcp.StreamableClientTransport{Endpoint: url}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	go func() {
		_, _ = session.CallTool(context.Background(),
			&mcp.CallToolParams{Name: "place"})
	}()
	<-started

	stop()

	select {
	case err := <-done:
		require.FailNow(t, "serve returned while the tool was running",
			"serve: %v", err)
	case <-time.After(tm.drain + 200*time.Millisecond):
	}
	close(release)
	require.NoError(t, waitServe(t, done, tm.drain+tm.final))
	select {
	case <-finished:
	default:
		assert.Fail(t, "serve returned before the tool finished")
	}
	assert.NotContains(t, logs.String(), "level=WARN")
}

// The SDK ends a tool's context with its request only on protocol
// 2026-07-28 or later. A tool registered through mcptool.Add sees the
// shutdown cut on any protocol, here a call naming none, so work it has
// not detached stops there as an ACP request's does.
func TestServeCutsAToolOnAnOldProtocol(t *testing.T) {
	tm := timing{
		read: time.Minute, drain: 50 * time.Millisecond,
		final: 5 * time.Second,
	}
	started := make(chan struct{})
	causes := make(chan error, 1)
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	mcptool.Add(server, slog.New(slog.DiscardHandler), mcptool.ServerMCP,
		&mcp.Tool{Name: "place"}, func(
			ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
		) (*mcp.CallToolResult, struct{}, error) {
			close(started)
			select {
			case <-ctx.Done():
				causes <- context.Cause(ctx)
			case <-time.After(tm.final):
				causes <- nil
			}
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	ctx, stop := stoppable(t)
	url, logs, done := startServe(t, ctx, mcpHandler(server), tm)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodPost, url, strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
					`"params":{"name":"place","arguments":{}}}`))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	stop()

	assert.ErrorIs(t, <-causes, httpmw.ErrShuttingDown)
	require.NoError(t, waitServe(t, done, tm.drain+tm.final))
	assert.NotContains(t, logs.String(), "level=WARN")
}

// mcpHandler serves server as /mcp and /mcp/admin do.
func mcpHandler(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			PropagateRequestCancellation: true,
		})
}
