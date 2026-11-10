// Package feeds generates per-tenant product feeds (Google Merchant, Meta,
// TikTok RSS dialect and the ACP JSON feed) from the Django catalog with a
// bounded-memory concurrent fetch, cached gzipped in Redis with
// stale-while-revalidate semantics.
package feeds

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

const (
	pageSize = 100
	// maxPages caps the catalog sweep; combined with pageSize this is a
	// 10k ceiling, logged per generation so truncation is never silent.
	maxPages = 100
	// fetchWorkers pages in flight; emitWindow completed-but-unemitted
	// pages, bounding memory to ~emitWindow*pageSize products.
	fetchWorkers = 4
	emitWindow   = 8
)

// fetchAllProducts streams the catalog in page order through emit. Returns
// the number of products emitted and whether the page cap truncated the
// sweep.
func fetchAllProducts(
	ctx context.Context,
	dj *django.Client,
	host, lang string,
	emit func(django.ProductFeed) error,
) (count int, truncated bool, err error) {
	first, err := dj.ListProductsPage(ctx, host, lang, 1, pageSize)
	if err != nil {
		return 0, false, fmt.Errorf("feeds: page 1: %w", err)
	}
	for _, p := range first.Results {
		if err := emit(p); err != nil {
			return count, false, err
		}
		count++
	}
	totalPages := first.TotalPages
	if totalPages <= 1 {
		return count, false, nil
	}
	truncated = totalPages > maxPages
	if truncated {
		totalPages = maxPages
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchWorkers)

	slots := make(map[int]chan []django.ProductFeed, totalPages)
	for p := 2; p <= totalPages; p++ {
		slots[p] = make(chan []django.ProductFeed, 1)
	}
	// window tokens are released only after a page is EMITTED, hard-
	// bounding completed-but-unconsumed pages even if emission stalls.
	window := make(chan struct{}, emitWindow)

	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for p := 2; p <= totalPages; p++ {
			select {
			case window <- struct{}{}:
			case <-gctx.Done():
				return
			}
			// select picks at random when the sweep has also ended.
			if gctx.Err() != nil {
				return
			}
			page := p
			g.Go(func() error {
				res, err := dj.ListProductsPage(gctx, host, lang, page, pageSize)
				if err != nil {
					return fmt.Errorf("feeds: page %d: %w", page, err)
				}
				slots[page] <- res.Results
				return nil
			})
		}
	}()
	// stop ends the sweep on every return. The producer must be done
	// before Wait (a g.Go racing Wait panics the WaitGroup), and the
	// fetches after it, so that no goroutine outlives the sweep.
	stop := func() error {
		cancel()
		<-produced
		return g.Wait()
	}

	for p := 2; p <= totalPages; p++ {
		select {
		case products := <-slots[p]:
			for _, prod := range products {
				if err := emit(prod); err != nil {
					_ = stop()
					return count, truncated, err
				}
				count++
			}
			<-window
		case <-gctx.Done():
			// A fetch failed: surface the group's error.
			if err := stop(); err != nil {
				return count, truncated, err
			}
			return count, truncated, gctx.Err()
		}
	}
	return count, truncated, stop()
}
