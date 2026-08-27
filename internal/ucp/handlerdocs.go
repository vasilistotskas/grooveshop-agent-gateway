package ucp

import (
	"embed"
	"io/fs"
	"net/http"
)

// Handler documents ship in the binary so the schema a platform fetches
// is exactly the one this build validates against — a separately
// deployed copy could drift from the advertised version.
//
//go:embed all:handlerdocs
var handlerDocs embed.FS

// HandlerDocsHandler serves the payment handler's spec page and JSON
// schemas at the versioned paths BuildProfile advertises.
//
// These documents are platform artifacts, identical in every
// environment, so every deployment advertises the one production host:
// the authority binding pins the schema origin to a host whose reversed
// labels equal HandlerName, and no per-environment hostname can satisfy
// that. Only `config.environment` distinguishes the deployments.
func HandlerDocsHandler() http.Handler {
	sub, err := fs.Sub(handlerDocs, "handlerdocs")
	if err != nil {
		// Impossible: the directory is embedded at compile time.
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Version-pinned immutable documents: a platform may cache them
		// for as long as it likes, since a change ships a new version
		// path rather than new bytes at the same URL.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
