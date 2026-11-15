package mcpsrv

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A review's comment is a shopper's text with no length of its own in the
// store. The tool bounds it, so one long review is not carried whole into
// every chat turn that reads the product's reviews.
func TestProductReviewsBoundTheComment(t *testing.T) {
	long := strings.Repeat("πολύ καλό προϊόν ", 400)
	reviews, err := json.Marshal(map[string]any{
		"count": 2,
		"results": []map[string]any{
			{"id": 1, "rate": 9, "createdAt": "2026-01-30T21:15:21+02:00",
				"user": map[string]any{"firstName": "Μαρία"},
				"translations": map[string]any{
					"el": map[string]any{"comment": long}}},
			{"id": 2, "rate": 7, "createdAt": "2026-01-31T10:00:00+02:00",
				"user": map[string]any{"firstName": "Νίκος"},
				"translations": map[string]any{
					"el": map[string]any{"comment": "Καλό."}}},
		},
	})
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/reviews") {
				_, _ = w.Write(reviews)
				return
			}
			_, _ = w.Write([]byte(`{"id": 1, "active": true}`))
		}))
	t.Cleanup(srv.Close)
	h := &handlers{deps: Deps{Site: testSite, Django: django.New(
		srv.URL+"/api/v1", "api.example.test", "secret", 5*time.Second,
		slog.New(slog.DiscardHandler), nil)}}
	store := cartTenant()
	store.Features = EveryFeature

	_, out, err := h.getProductReviews(
		tenant.NewContext(t.Context(), store), nil, ReviewsIn{ProductID: 1})

	require.NoError(t, err)
	require.Len(t, out.Reviews, 2)
	cut := out.Reviews[0].Comment
	assert.LessOrEqual(t, utf8.RuneCountInString(cut), reviewCommentMax+1)
	assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(cut, "…")),
		"the comment's own words, cut short")
	assert.True(t, strings.HasSuffix(cut, "…"), "the cut is marked")
	assert.Equal(t, "Καλό.", out.Reviews[1].Comment)
}
