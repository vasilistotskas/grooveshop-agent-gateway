package feeds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

var discard = slog.New(slog.DiscardHandler)

// pagedCatalog serves totalPages pages of one product each, answers 404
// for failPage (0: none), and counts the page requests it answered.
func pagedCatalog(
	t *testing.T, totalPages, failPage int,
) (*django.Client, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			pages.Add(1)
			page := r.URL.Query().Get("page")
			if page == strconv.Itoa(failPage) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writePage(w, totalPages, page)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics()), &pages
}

// writePage answers one catalogue page holding one product.
func writePage(w http.ResponseWriter, totalPages int, page string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"links":{"next":null,"previous":null},
		"count":%d,"totalPages":%d,"pageSize":1,"page":%s,
		"results":[{"id":%s,"translations":{"el":{"name":"P"}},
		"slug":"p","category":1,"effectivePrice":1.24,
		"priorPrice":null,"announcedDiscountPercent":null,"stock":1,
		"active":true,"mainImagePath":"m.jpg"}]}`,
		totalPages, totalPages, page, page)
}

// panicking is a log handler that panics. Given to the Django client,
// it panics on the goroutine fetching a page whose request failed.
type panicking struct{}

func (panicking) Enabled(context.Context, slog.Level) bool { return true }

func (panicking) Handle(context.Context, slog.Record) error {
	panic("boom")
}

func (h panicking) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h panicking) WithGroup(string) slog.Handler { return h }

func TestFetchAllProductsStreamsEveryPageInOrder(t *testing.T) {
	dj, pages := pagedCatalog(t, 5, 0)
	var ids []int64
	count, truncated, err := fetchAllProducts(context.Background(), dj, discard,
		"shop.example.test", "el", func(p django.ProductFeed) error {
			ids = append(ids, p.ID)
			return nil
		})
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, 5, count)
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, ids)
	assert.EqualValues(t, 5, pages.Load())
}

// An emit failure must surface promptly, and the sweep must stop
// fetching: only the pages already in flight when the consumer gave up
// reach Django, never the whole catalog.
func TestFetchAllProductsEmitErrorStopsTheSweep(t *testing.T) {
	dj, pages := pagedCatalog(t, 40, 0)
	boom := errors.New("boom")
	done := make(chan error, 1)
	go func() {
		_, _, err := fetchAllProducts(context.Background(), dj, discard,
			"shop.example.test", "el", func(p django.ProductFeed) error {
				if p.ID == 2 {
					return boom
				}
				return nil
			})
		done <- err
	}()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, boom)
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not return after the emit error")
	}
	// Bounded by the fetch window plus the workers that were mid-flight.
	assert.LessOrEqual(t, int(pages.Load()), 1+emitWindow+fetchWorkers)
}

// A page failing mid-sweep fails the sweep with its error. The producer
// used to start the next fetch while the consumer waited on the group,
// which panicked its WaitGroup. That race is narrow, about one sweep in
// several thousand, so one run repeats the sweep, on one server and
// client so that it reuses their connections: a lot of sweeps per
// second, though one run still catches the old race only now and then.
func TestFetchAllProductsFailedPageFailsTheSweep(t *testing.T) {
	dj, _ := pagedCatalog(t, 60, 7)
	for range 750 {
		_, _, err := fetchAllProducts(context.Background(), dj, discard,
			"shop.example.test", "el",
			func(django.ProductFeed) error { return nil })
		require.ErrorIs(t, err, django.ErrNotFound)
	}
}

// A panic while fetching a page fails the sweep with that page's error,
// instead of ending the process from the errgroup's goroutine.
func TestFetchAllProductsPanickingPageFailsTheSweep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page != "1" {
				// The connection drops, and the client logs the failure.
				panic(http.ErrAbortHandler)
			}
			writePage(w, 2, page)
		}))
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, slog.New(panicking{}), obs.NewMetrics())
	_, _, err := fetchAllProducts(context.Background(), dj, discard,
		"shop.example.test", "el",
		func(django.ProductFeed) error { return nil })
	require.EqualError(t, err, "feeds: page 2: panicked")
}
