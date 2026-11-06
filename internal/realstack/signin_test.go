//go:build realstack

package realstack

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The redirect a native client registers. Nothing listens there: the sign-in
// stops at the redirect and reads the code from it.
const callbackURL = "http://127.0.0.1:1/callback"

// django is a browser's view of the store's API host: https URLs and secure
// cookies, while the bytes go to the container the way the ingress sends them,
// plain http with X-Forwarded-Proto.
type djangoTransport struct{ endpoint *url.URL }

func (d djangoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	sent := r.Clone(r.Context())
	sent.Host = r.URL.Host
	sent.URL.Scheme = d.endpoint.Scheme
	sent.URL.Host = d.endpoint.Host
	sent.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultTransport.RoundTrip(sent)
	if err != nil {
		return nil, err
	}
	// The caller's URL, not the container's: forms resolve against it.
	resp.Request = r
	return resp, nil
}

func browser(t *testing.T) *http.Client {
	t.Helper()
	endpoint, err := running.django.PortEndpoint(
		context.Background(), "8000/tcp", "http")
	require.NoError(t, err)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{
		Timeout:   requestTimeout,
		Jar:       jar,
		Transport: djangoTransport{target},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if strings.HasPrefix(req.URL.String(), callbackURL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

var (
	formTag  = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	inputTag = regexp.MustCompile(`(?is)<input\b[^>]*>`)
)

func attr(tag, name string) string {
	m := regexp.MustCompile(`(?is)\b` + name + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// formFields reads a page's POST form: where it posts, and its hidden inputs.
func formFields(t *testing.T, page string) (string, url.Values) {
	t.Helper()
	var action string
	for _, tag := range formTag.FindAllString(page, -1) {
		if strings.EqualFold(attr(tag, "method"), "post") {
			action = attr(tag, "action")
			break
		}
	}
	values := url.Values{}
	for _, tag := range inputTag.FindAllString(page, -1) {
		if strings.EqualFold(attr(tag, "type"), "hidden") {
			values.Set(attr(tag, "name"), attr(tag, "value"))
		}
	}
	require.NotEmpty(t, values.Get("csrfmiddlewaretoken"), "no form on:\n%s", tail(page))
	return action, values
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func postForm(
	t *testing.T, c *http.Client, from *url.URL, action string, values url.Values,
) *http.Response {
	t.Helper()
	target, err := from.Parse(action)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, target.String(),
		strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Django's CSRF check on a secure request needs the page it came from.
	req.Header.Set("Referer", from.String())
	resp, err := c.Do(req)
	require.NoError(t, err)
	return resp
}

// signIn opens an authorize URL as an anonymous shopper and signs in, which
// leads back to it. It returns what the authorize step then answers.
func signIn(t *testing.T, c *http.Client, authorize string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(authorize)
	require.NoError(t, err)
	page := read(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	assert.Contains(t, resp.Request.URL.Path, "/accounts/login",
		"an anonymous shopper signs in first")
	action, form := formFields(t, page)
	form.Set("login", running.seeded.BuyerEmail)
	form.Set("password", running.seeded.BuyerPassword)
	resp = postForm(t, c, resp.Request.URL, action, form)
	page = read(t, resp)
	assert.Contains(t, resp.Request.URL.Path, "/identity/o/authorize",
		"signing in leads back to the authorize step")
	return resp, page
}

// authServer is the store's OAuth metadata, as a client reads it.
type authServer struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	Registration  string `json:"registration_endpoint"`
}

// registerClient reads the metadata and registers a client the way a real one
// does, through dynamic client registration.
func registerClient(t *testing.T, c *http.Client) (authServer, string) {
	t.Helper()
	resp, err := c.Get("https://" + apiHost + "/.well-known/oauth-authorization-server")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var server authServer
	require.NoError(t, json.Unmarshal([]byte(read(t, resp)), &server))

	registration, err := json.Marshal(map[string]any{
		"client_name":                "Real-stack sign-in",
		"scope":                      "profile orders:read",
		"redirect_uris":              []string{callbackURL},
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	require.NoError(t, err)
	resp, err = c.Post(server.Registration, "application/json",
		strings.NewReader(string(registration)))
	require.NoError(t, err)
	body := read(t, resp)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var registered struct {
		ID string `json:"client_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &registered))
	return server, registered.ID
}

// authorizeURL is the sign-in link a client sends the shopper to.
func authorizeURL(server authServer, clientID, resource, verifier string) string {
	challenge := sha256.Sum256([]byte(verifier))
	return server.Authorization + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {callbackURL},
		"scope":                 {"profile orders:read"},
		"state":                 {"real-stack"},
		"resource":              {resource},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}.Encode()
}

// A real client's whole path: it reads the store's metadata, registers itself,
// sends the shopper to sign in and consent, redeems the code with PKCE, and
// uses the token it was given.
func TestRealSignInLinksAnAccount(t *testing.T) {
	c := browser(t)
	store := "https://" + storeHost + "/mcp"
	server, clientID := registerClient(t, c)

	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	verifier := base64.RawURLEncoding.EncodeToString(raw)

	resp, page := signIn(t, c, authorizeURL(server, clientID, store, verifier))
	refused, _ := resp.Location()
	require.Equal(t, http.StatusOK, resp.StatusCode, "%v\n%s", refused, tail(page))

	action, form := formFields(t, page)
	require.NotEmpty(t, form.Get("request"), "no consent form on:\n%s", tail(page))
	form.Set("action", "grant")
	form["scopes"] = []string{"profile", "orders:read"}
	resp = postForm(t, c, resp.Request.URL, action, form)
	read(t, resp)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	location, err := resp.Location()
	require.NoError(t, err)
	code := location.Query().Get("code")
	require.NotEmpty(t, code, "redirected to %s", location)
	assert.Equal(t, "real-stack", location.Query().Get("state"))

	resp, err = c.PostForm(server.Token, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {callbackURL},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {store},
	})
	require.NoError(t, err)
	body = read(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var token struct {
		Access string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &token))
	require.NotEmpty(t, token.Access)

	s := connect(t, token.Access)
	structured(t, call(t, s, "my_orders", map[string]any{}))
}

func TestAuthorizeRefusesAResourceThatIsNotTheStore(t *testing.T) {
	c := browser(t)
	server, clientID := registerClient(t, c)
	const verifier = "real-stack-verifier-long-enough-for-the-pkce-rule-0123"

	// allauth renders the refusal as a page, so what proves it is the consent
	// form: absent for another resource, present for the same client and
	// request naming the store's own.
	resp, page := signIn(t, c, authorizeURL(
		server, clientID, "https://other.example/mcp", verifier))
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	assert.NotContains(t, page, `name="request"`, "no consent form")

	resp, err := c.Get(authorizeURL(
		server, clientID, "https://"+storeHost+"/mcp", verifier))
	require.NoError(t, err)
	page = read(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	assert.Contains(t, page, `name="request"`, "the store's own gets consent")
}
