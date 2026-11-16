//go:build realstack

package realstack

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// platform is a UCP platform that signs its requests to the gateway with
// a key its profile publishes, served by the stack's profile container.
type platform struct {
	key        *ucp.SigningKey
	profileURL string
	// keyFile holds the key as cmd/ucp-sign reads it, for the runbook's
	// commands.
	keyFile string
}

// newPlatform makes a platform whose profile, the runbook's with the
// platform's key in keys[], the profile container serves as name. It
// returns the profile's bytes; remove deletes the key file.
func newPlatform(name string) (platform, []byte, func(), error) {
	none := func() {}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return platform{}, nil, none, err
	}
	scalar, err := priv.Bytes()
	if err != nil {
		return platform{}, nil, none, err
	}
	key, err := ucp.SigningKeyFromScalar(scalar)
	if err != nil {
		return platform{}, nil, none, err
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "runbook_platform_profile.json"))
	if err != nil {
		return platform{}, nil, none, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return platform{}, nil, none, err
	}
	doc["keys"] = []map[string]string{key.JWK()}
	profile, err := json.Marshal(doc)
	if err != nil {
		return platform{}, nil, none, err
	}
	f, err := os.CreateTemp("", "realstack-platform-*.key")
	if err != nil {
		return platform{}, nil, none, err
	}
	remove := func() { _ = os.Remove(f.Name()) }
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(scalar))
	if cerr := f.Close(); werr != nil || cerr != nil {
		remove()
		if werr != nil {
			return platform{}, nil, none, werr
		}
		return platform{}, nil, none, cerr
	}
	return platform{
		key:        key,
		profileURL: "http://profile:8090/" + name,
		keyFile:    f.Name(),
	}, profile, remove, nil
}

// signedTransport is hostTransport with every request signed by the
// platform, as UCP's MCP transport binding asks (ucp.SignRequest).
type signedTransport struct {
	hostTransport
	platform platform
}

func (s signedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = s.host
	var body []byte
	if r.Body != nil {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if err := ucp.SignRequest(r, body, s.platform.profileURL,
		s.platform.key, time.Now()); err != nil {
		return nil, err
	}
	return s.hostTransport.RoundTrip(r)
}
