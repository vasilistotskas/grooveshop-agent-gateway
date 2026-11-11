package feeds

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httphdr"
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
		unavailable := func(err error) {
			s.log.WarnContext(r.Context(), "feed unavailable",
				slog.String("kind", kind),
				slog.String("error", err.Error()),
			)
			http.Error(w, "feed temporarily unavailable",
				http.StatusServiceUnavailable)
		}
		meta, err := s.Meta(r.Context(), t, kind)
		if err != nil {
			unavailable(err)
			return
		}

		h := w.Header()
		cacheable := func(etag string) {
			h.Set("ETag", etag)
			h.Set("Cache-Control", "public, max-age=3600")
			h.Set("Content-Type", contentType)
			h.Set("Vary", "Accept-Encoding")
		}
		ifNoneMatch := strings.Join(r.Header.Values("If-None-Match"), ",")
		if httphdr.ETagMatches(ifNoneMatch, meta.ETag) {
			cacheable(meta.ETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// An absent Accept-Encoding accepts any coding (RFC 9110
		// 12.5.3), but a client that sent none may not decode gzip, so
		// it gets identity like an empty one.
		acceptEncoding := strings.Join(r.Header.Values("Accept-Encoding"), ",")
		gzipWeight := httphdr.EncodingWeight(acceptEncoding, httphdr.Gzip)
		identityWeight := httphdr.EncodingWeight(acceptEncoding,
			httphdr.Identity)
		if gzipWeight == 0 && identityWeight == 0 {
			h.Set("Accept-Encoding", httphdr.Gzip)
			http.Error(w, "feed served gzip or identity only",
				http.StatusUnsupportedMediaType)
			return
		}

		gz, meta, err := s.Body(r.Context(), t.SchemaName, kind)
		if err != nil {
			unavailable(err)
			return
		}
		cacheable(meta.ETag)
		if gzipWeight > 0 && gzipWeight >= identityWeight {
			h.Set("Content-Encoding", httphdr.Gzip)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gz)
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			http.Error(w, "feed corrupt", http.StatusInternalServerError)
			return
		}
		defer func() { _ = zr.Close() }()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, zr) //nolint:gosec // bounded: our own cache payload
	})
}
