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
	// 10k ceiling. Unlike the old Nuxt implementation the cap is logged
	// per generation (no silent truncation) and pages fetch concurrently.
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
	emit func(django.Product) error,
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

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchWorkers)

	type slot struct {
		ch chan []django.Product
	}
	slots := make(map[int]slot, totalPages)
	for p := 2; p <= totalPages; p++ {
		slots[p] = slot{ch: make(chan []django.Product, 1)}
	}
	// window tokens are released only after a page is EMITTED, hard-
	// bounding completed-but-unconsumed pages even if emission stalls.
	window := make(chan struct{}, emitWindow)

	go func() {
		for p := 2; p <= totalPages; p++ {
			select {
			case window <- struct{}{}:
			case <-gctx.Done():
				return
			}
			page := p
			g.Go(func() error {
				res, err := dj.ListProductsPage(gctx, host, lang, page, pageSize)
				if err != nil {
					return fmt.Errorf("feeds: page %d: %w", page, err)
				}
				slots[page].ch <- res.Results
				return nil
			})
		}
	}()

	for p := 2; p <= totalPages; p++ {
		select {
		case products := <-slots[p].ch:
			for _, prod := range products {
				if err := emit(prod); err != nil {
					return count, truncated, err
				}
				count++
			}
			<-window
		case <-gctx.Done():
			// A fetch failed: surface the group's error.
			if err := g.Wait(); err != nil {
				return count, truncated, err
			}
			return count, truncated, gctx.Err()
		}
	}
	if err := g.Wait(); err != nil {
		return count, truncated, err
	}
	return count, truncated, nil
}
