package feeds

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Handler serves one feed kind. Route registration binds the kind, keeping
// mux patterns literal (/feeds/google.xml etc.) for metrics labels.
func (s *Service) Handler(kind string) http.Handler {
	contentType := "application/xml; charset=utf-8"
	if kind == KindACP {
		contentType = "application/json; charset=utf-8"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenant.FromContext(r.Context())
		if !ok {
			http.Error(w, "unknown store", http.StatusNotFound)
			return
		}
		gz, meta, err := s.Get(r.Context(), t, kind)
		if err != nil {
			s.log.ErrorContext(r.Context(), "feed generation failed",
				slog.String("tenant", t.SchemaName),
				slog.String("kind", kind),
				slog.String("error", err.Error()),
			)
			http.Error(w, "feed temporarily unavailable",
				http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("ETag", meta.ETag)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Vary", "Accept-Encoding")
		if r.Header.Get("If-None-Match") == meta.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gz)
			return
		}
		zr, err := gzip.NewReader(strings.NewReader(string(gz)))
		if err != nil {
			http.Error(w, "feed corrupt", http.StatusInternalServerError)
			return
		}
		defer func() { _ = zr.Close() }()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, zr) //nolint:gosec // bounded: our own cache payload
	})
}
