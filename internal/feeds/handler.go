package feeds

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"

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
		if httphdr.ETagMatches(r.Header.Values("If-None-Match"), meta.ETag) {
			cacheable(meta.ETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		gz, meta, err := s.Body(r.Context(), t, kind)
		if err != nil {
			unavailable(err)
			return
		}
		cacheable(meta.ETag)
		if servesGzip(r.Header.Values("Accept-Encoding")) {
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

// servesGzip picks the feed's coding from Accept-Encoding: gzip whenever
// it weighs above 0, unless identity is listed with a higher weight;
// identity not listed is only acceptable by default and weighs nothing
// against it (RFC 9110 12.5.3). A request that accepts neither still
// gets identity: 12.5.3 makes refused identity the exception to sending
// no coding, but 12.1 lets the server decide "that sending a response
// that doesn't conform to the user agent's preferences is better than
// sending a 406". An absent field
// accepts any coding (12.5.3 rule 1); the feed then goes uncompressed,
// as before, because a client that names no coding may not decode one.
func servesGzip(acceptEncoding []string) bool {
	if len(acceptEncoding) == 0 {
		return false
	}
	gz, _ := httphdr.EncodingWeight(acceptEncoding, httphdr.Gzip)
	id, idListed := httphdr.EncodingWeight(acceptEncoding, httphdr.Identity)
	return gz > 0 && !(idListed && id > gz)
}
