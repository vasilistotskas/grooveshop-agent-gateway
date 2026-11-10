package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
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

// startServe runs serve on a loopback port and returns its address, the
// stop signal and serve's result.
func startServe(
	t *testing.T, handler http.Handler, window shutdownWindow,
	logs io.Writer,
) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, handler, window,
			slog.New(slog.NewTextHandler(logs, nil)))
	}()
	return "http://" + ln.Addr().String(), stop, done
}

// openStream starts a request and returns its body once the handler has
// flushed the first line.
func openStream(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body := bufio.NewReader(resp.Body)
	line, err := body.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "open\n", line)
	return body
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
	window := shutdownWindow{
		drain: 100 * time.Millisecond, final: 5 * time.Second,
	}
	causes := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "open\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		causes <- context.Cause(r.Context())
		_, _ = io.WriteString(w, "final\n")
	})
	url, stop, done := startServe(t, handler, window, io.Discard)
	body := openStream(t, url)

	began := time.Now()
	stop()

	require.NoError(t, waitServe(t, done, window.drain+window.final))
	assert.GreaterOrEqual(t, time.Since(began), window.drain,
		"the request kept a live context through the drain")
	assert.ErrorIs(t, <-causes, httpmw.ErrShuttingDown)
	rest, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "final\n", string(rest))
}

// A handler that ignores the cancellation is cut when the window ends,
// and that is a warning, not a failed exit.
func TestServeCutsWhatOutlivesTheWindow(t *testing.T) {
	window := shutdownWindow{
		drain: 50 * time.Millisecond, final: 50 * time.Millisecond,
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "open\n")
		_ = http.NewResponseController(w).Flush()
		<-release
	})
	logs := &lockedBuffer{}
	url, stop, done := startServe(t, handler, window, logs)
	body := openStream(t, url)

	stop()

	require.NoError(t, waitServe(t, done, 5*time.Second))
	_, err := io.ReadAll(body)
	assert.Error(t, err, "the cut stream ends without its chunked end")
	assert.Contains(t, logs.String(), "level=WARN")
}

// A listener that fails is still an error.
func TestServeReturnsAListenerFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	err = serve(context.Background(), ln, http.NotFoundHandler(),
		podShutdown, slog.New(slog.DiscardHandler))

	assert.Error(t, err)
}
