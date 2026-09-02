package feeds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// pagedCatalog serves totalPages pages of one product each and counts
// the page requests it answered.
func pagedCatalog(t *testing.T, totalPages int) (*django.Client, *atomic.Int32) {
	t.Helper()
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			pages.Add(1)
			page := r.URL.Query().Get("page")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"links":{"next":null,"previous":null},
				"count":%d,"totalPages":%d,"pageSize":1,"page":%s,
				"results":[{"id":%s,"translations":{"el":{"name":"P"}},
				"slug":"p","category":1,"price":1.00,"vatValue":0.24,
				"finalPrice":1.24,"discountPercent":0.0,"stock":1,
				"active":true,"mainImagePath":"m.jpg"}]}`,
				totalPages, totalPages, page, page)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics()), &pages
}

func TestFetchAllProductsStreamsEveryPageInOrder(t *testing.T) {
	dj, pages := pagedCatalog(t, 5)
	var ids []int64
	count, truncated, err := fetchAllProducts(context.Background(), dj,
		"shop.example.test", "el", func(p django.Product) error {
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
	dj, pages := pagedCatalog(t, 40)
	boom := errors.New("boom")
	done := make(chan error, 1)
	go func() {
		_, _, err := fetchAllProducts(context.Background(), dj,
			"shop.example.test", "el", func(p django.Product) error {
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
