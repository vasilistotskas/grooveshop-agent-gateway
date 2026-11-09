//go:build realstack

package realstack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store-admin MCP server (/mcp/admin) on the real stack: the staff
// issuer on the connect host, Django's role and team checks, the gateway's
// verification, and the refusals that keep a customer and another store's
// staff out.

const staffPassword = "real-stack-staff-passw0rd"

var staffResource = "https://" + storeHost + "/mcp/admin"

var staffScopes = []string{
	"admin:orders:read", "admin:sales:read", "admin:profit:read",
	"admin:catalog:read", "admin:customers:read", "admin:reviews:read",
	"admin:messages:read",
}

// managePy runs a Django shell script in the running backend.
func managePy(t *testing.T, script string) {
	t.Helper()
	code, out, err := running.django.Exec(context.Background(), []string{
		".venv/bin/python", "manage.py", "shell", "-c", script,
	})
	require.NoError(t, err)
	output, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, tail(string(output)))
}

// staffMember creates a platform identity with a role in the store, or in
// no store when role is empty.
func staffMember(t *testing.T, email, role string) {
	t.Helper()
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import get_public_schema_name, schema_context
from tenant.models import Tenant, UserTenantMembership
from user.models import UserAccount
with schema_context(get_public_schema_name()):
    user = UserAccount.objects.create_user(
        email=%q, password=%q, is_staff=True)
    if %q:
        UserTenantMembership.objects.create(
            user=user, tenant=Tenant.objects.get(schema_name="demostore"),
            role=%q, is_active=True)
`, email, staffPassword, role, role))
}

// staffClient registers a client with the staff issuer, through dynamic
// client registration, as a real AI app does.
func staffClient(t *testing.T, c *http.Client) (authServer, string) {
	t.Helper()
	resp, err := c.Get("https://" + connectHost +
		"/.well-known/oauth-authorization-server")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var server authServer
	require.NoError(t, json.Unmarshal([]byte(read(t, resp)), &server))

	registration, err := json.Marshal(map[string]any{
		"client_name":                "Real-stack staff app",
		"redirect_uris":              []string{callbackURL},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(staffScopes, " ") + " offline_access",
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

func staffAuthorizeURL(server authServer, clientID, verifier string) string {
	u := authorizeURL(server, clientID, staffResource, verifier)
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	q.Set("scope", strings.Join(staffScopes, " ")+" offline_access")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// staffSignIn opens the authorize URL, signs in on the connect host's
// staff page and returns what the authorize step answers next.
func staffSignIn(
	t *testing.T, c *http.Client, authorize, email string,
) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(authorize)
	require.NoError(t, err)
	page := read(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	require.Equal(t, "/identity/staff/login/", resp.Request.URL.Path,
		"staff sign in on the connect host's own page")
	action, form := formFields(t, page)
	form.Set("username", email)
	form.Set("password", staffPassword)
	resp = postForm(t, c, resp.Request.URL, action, form)
	return resp, read(t, resp)
}

func verifier(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func connectAdmin(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	session, err := mcp.NewClient(
		&mcp.Implementation{Name: "real-stack-staff", Version: "test"}, nil,
	).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: running.gatewayURL + "/mcp/admin",
		HTTPClient: &http.Client{
			Timeout:   requestTimeout,
			Transport: hostTransport{storeHost, token},
		},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// adminCall posts one tools/call to /mcp/admin as a client with token
// would, and returns the HTTP answer.
func adminCall(t *testing.T, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		running.gatewayURL+"/mcp/admin",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"list_orders","arguments":{}}}`))
	require.NoError(t, err)
	req.Host = storeHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A staff member connects an AI app end to end: discovery from the store,
// registration and sign-in at the connect host, a consent cut to the role,
// the code redeemed with PKCE, and the store read through /mcp/admin.
func TestStaffConnectsAnAIAppAndReadsTheStore(t *testing.T) {
	staffMember(t, "staff-member@realstack.test", "staff")

	// What a client reads first: the store names the staff issuer.
	resp, err := (&http.Client{
		Timeout: requestTimeout, Transport: hostTransport{storeHost, ""},
	}).Get(running.gatewayURL +
		"/.well-known/oauth-protected-resource/mcp/admin")
	require.NoError(t, err)
	var prm struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	require.NoError(t, json.Unmarshal([]byte(read(t, resp)), &prm))
	assert.Equal(t, staffResource, prm.Resource)
	require.Equal(t, []string{"https://" + connectHost}, prm.Servers)

	c := browser(t)
	server, clientID := staffClient(t, c)
	v := verifier(t)
	resp, page := staffSignIn(t, c,
		staffAuthorizeURL(server, clientID, v), "staff-member@realstack.test")
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	require.Contains(t, page, `name="request"`, "consent:\n%s", tail(page))
	assert.Contains(t, page, `value="admin:orders:read"`)
	assert.NotContains(t, page, `value="admin:sales:read"`,
		"a STAFF role is never offered the takings")
	assert.NotContains(t, page, `value="admin:profit:read"`)

	action, form := formFields(t, page)
	form.Set("action", "grant")
	form["scopes"] = []string{"admin:orders:read", "admin:catalog:read"}
	resp = postForm(t, c, resp.Request.URL, action, form)
	read(t, resp)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	location, err := resp.Location()
	require.NoError(t, err)
	code := location.Query().Get("code")
	require.NotEmpty(t, code, "redirected to %s", location)

	resp, err = c.PostForm(server.Token, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {callbackURL},
		"client_id":     {clientID},
		"code_verifier": {v},
		"resource":      {staffResource},
	})
	require.NoError(t, err)
	body := read(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var token struct {
		Access string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &token))

	s := connectAdmin(t, token.Access)
	me := structured(t, call(t, s, "whoami", map[string]any{}))
	assert.Equal(t, "staff", me["role"])
	structured(t, call(t, s, "list_orders", map[string]any{"limit": 5}))
	structured(t, call(t, s, "list_products", map[string]any{"limit": 5}))
	// Not granted at consent, and not open to the role.
	assert.Contains(t, errorText(t, call(t, s, "sales_report",
		map[string]any{})), `"code":"not_allowed"`)
	assert.Contains(t, errorText(t, call(t, s, "find_customers",
		map[string]any{})), `"code":"not_granted"`)
}

// Anyone without a staff token is sent to sign in, and a customer's token
// from the store's own issuer is not a staff token.
func TestAdminRefusesAnonymousAndCustomerTokens(t *testing.T) {
	resp := adminCall(t, "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	challenge := resp.Header.Get("WWW-Authenticate")
	assert.Contains(t, challenge, `scope="admin:orders:read`)
	assert.Contains(t, challenge, `resource_metadata="https://`+storeHost+
		`/.well-known/oauth-protected-resource/mcp/admin"`)

	// The shopper's own sign-in on the store's issuer, bound to the
	// store's /mcp as the shopper connector binds it.
	c := browser(t)
	server, clientID := registerClient(t, c, "profile orders:read")
	v := verifier(t)
	resp, page := signIn(t, c, authorizeURL(
		server, clientID, "https://"+storeHost+"/mcp", v))
	require.Equal(t, http.StatusOK, resp.StatusCode, tail(page))
	action, form := formFields(t, page)
	form.Set("action", "grant")
	form["scopes"] = []string{"profile", "orders:read"}
	resp = postForm(t, c, resp.Request.URL, action, form)
	read(t, resp)
	location, err := resp.Location()
	require.NoError(t, err)
	resp, err = c.PostForm(server.Token, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {location.Query().Get("code")},
		"redirect_uri":  {callbackURL},
		"client_id":     {clientID},
		"code_verifier": {v},
		"resource":      {"https://" + storeHost + "/mcp"},
	})
	require.NoError(t, err)
	var token struct {
		Access string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal([]byte(read(t, resp)), &token))
	require.NotEmpty(t, token.Access)

	resp = adminCall(t, token.Access)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"),
		`error="invalid_token"`)
}

// Someone off the store's team is refused at consent: no form, no token.
func TestStaffOfNoStoreIsRefusedAtConsent(t *testing.T) {
	staffMember(t, "outsider@realstack.test", "")
	c := browser(t)
	server, clientID := staffClient(t, c)

	resp, page := staffSignIn(t, c,
		staffAuthorizeURL(server, clientID, verifier(t)),
		"outsider@realstack.test")

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, tail(page))
	assert.NotContains(t, page, `name="request"`)
}
