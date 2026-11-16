// Command ucp-sign is a UCP platform's signing client, for checking a
// running gateway: it signs requests to a store's /mcp per RFC 9421 with
// a key the platform's profile publishes, as UCP's MCP transport binding
// asks (ucp.SignRequest).
//
//	ucp-sign keygen -key FILE < profile.json > signed-profile.json
//
// makes a throwaway ES256 key, writes its private scalar to FILE (which
// must not exist), and prints the platform profile read on stdin with
// the key's public half in its keys[]. Publish that profile at a public
// https URL: the gateway fetches it to verify the signatures.
//
//	ucp-sign post -key FILE -profile URL -gateway URL [-host HOST] [-H 'Name: value']... < body.json
//
// signs the JSON-RPC body read on stdin under the profile URL and POSTs
// it to the gateway's /mcp, with Host set to HOST when given. It prints
// the response body, and the status on stderr when it is not 2xx.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// requestTimeout bounds one signed call: the gateway's own discovery
// fetch and Django calls finish well inside it.
const requestTimeout = time.Minute

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: ucp-sign keygen|post [flags]"))
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "post":
		err = post(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q: keygen or post", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ucp-sign:", err)
	os.Exit(1)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	keyFile := fs.String("key", "", "file to write the private key to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" {
		return errors.New("keygen needs -key")
	}
	var profile map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&profile); err != nil {
		return fmt.Errorf("read the profile on stdin: %w", err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	scalar, err := priv.Bytes()
	if err != nil {
		return err
	}
	key, err := ucp.SigningKeyFromScalar(scalar)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(scalar) + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	profile["keys"] = []map[string]string{key.JWK()}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	return out.Encode(profile)
}

// headerFlags collects repeated -H 'Name: value' flags.
type headerFlags []string

func (h *headerFlags) String() string     { return strings.Join(*h, ", ") }
func (h *headerFlags) Set(v string) error { *h = append(*h, v); return nil }

func post(args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	keyFile := fs.String("key", "", "file keygen wrote the private key to")
	profile := fs.String("profile", "", "URL of the signing platform's profile")
	gateway := fs.String("gateway", "", "base URL of the gateway")
	host := fs.String("host", "", "the store's host, when the gateway URL is not it")
	var headers headerFlags
	fs.Var(&headers, "H", "a request header, 'Name: value'; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" || *profile == "" || *gateway == "" {
		return errors.New("post needs -key, -profile and -gateway")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	scalar, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("the key file holds no key: %w", err)
	}
	key, err := ucp.SigningKeyFromScalar(scalar)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(*gateway, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if *host != "" {
		req.Host = *host
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("-H %q is not 'Name: value'", h)
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	if err := ucp.SignRequest(req, body, *profile, key, time.Now()); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintln(os.Stderr, "HTTP", resp.StatusCode)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
