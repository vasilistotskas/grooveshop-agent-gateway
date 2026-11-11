package feeds

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httphdr"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

const feedCacheControl = "public, max-age=3600"

// Handler serves one feed kind. Route registration binds the kind, keeping
// mux patterns literal (/feeds/google.xml etc.) for metrics labels. The
// GET patterns serve HEAD too, which gets the same header fields.
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

		// What a 304 carries (RFC 9110 15.4.5); a 200 adds the
		// representation's own fields.
		h := w.Header()
		validators := func(etag string) {
			h.Set("ETag", etag)
			h.Set("Cache-Control", feedCacheControl)
			h.Set("Vary", "Accept-Encoding")
		}
		if httphdr.ETagMatches(r.Header.Values("If-None-Match"), meta.ETag) {
			validators(meta.ETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		gz, meta, err := s.Body(r.Context(), t, kind)
		if err != nil {
			unavailable(err)
			return
		}
		validators(meta.ETag)
		h.Set("Content-Type", contentType)
		gzipped := servesGzip(r.Header.Values("Accept-Encoding"))
		size := meta.Size
		if gzipped {
			h.Set("Content-Encoding", httphdr.Gzip)
			size = len(gz)
		}
		// Body checked that gz decodes to meta.Size bytes, so the length
		// is what is written. A write that fails now is the client's
		// connection; net/http closes it short of this length, so the cut
		// is visible, never a complete-looking 200.
		h.Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		switch {
		case r.Method == http.MethodHead:
		case gzipped:
			_, _ = w.Write(gz)
		default:
			_ = decode(gz, meta.Size, w)
		}
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
