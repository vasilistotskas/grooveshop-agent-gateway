//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// languageDjango is the fake Django recording the X-Language of each
// product call.
func languageDjango(t *testing.T) (http.Handler, func() []string) {
	t.Helper()
	base := fakeDjangoMux(t)
	var mu sync.Mutex
	var langs []string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/product/1" {
				mu.Lock()
				langs = append(langs, r.Header.Get("X-Language"))
				mu.Unlock()
			}
			base.ServeHTTP(w, r)
		}), func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), langs...)
		}
}

// On /en an English question was answered in Greek, with Greek product
// names: the request carried no language. A language the store serves
// (the fixture store: el, en) is the turn's — in the prompt and in every
// Django call its tools make; one it does not serve is ignored for the
// store's default.
func TestChatServesTheTurnInThePagesLanguage(t *testing.T) {
	for name, tc := range map[string]struct {
		locale     string
		wantLang   string
		wantPrompt string
		wantName   string
		// enLinks: the product links name English (/en/...); the
		// storefront's build default, el, is unprefixed.
		enLinks bool
	}{
		"served": {"en", "en", "reply in English (en)",
			"Scented Candles", true},
		"not served": {"de", "el", "reply in Greek (el)",
			"Portable Phone Charger", false},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeChatAPI{scripts: []string{
				toolUseTurnSSE("get_product", `{"productId": 1}`),
				textTurnSSE("OK."),
			}}
			djangoH, langs := languageDjango(t)
			gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

			events := postChat(t, gw.URL, map[string]any{
				"message": "show me product 1", "locale": tc.locale,
			})

			lastEvent(t, events, "done")
			assert.Equal(t, []string{tc.wantLang}, langs())
			fake.mu.Lock()
			first := string(fake.bodies[0])
			fake.mu.Unlock()
			assert.Contains(t, first, tc.wantPrompt)
			last := string(*fake.lastBody.Load())
			assert.Contains(t, last, tc.wantName)
			assert.Equal(t, tc.enLinks,
				strings.Contains(last, "/en/products/1/"), last)
			assert.Contains(t, last, "/products/1/")
		})
	}
}

// The widget shows a refusal verbatim, so it speaks the turn's language.
func TestChatRefusesInThePagesLanguage(t *testing.T) {
	fake := &fakeChatAPI{}
	gw := startChatGateway(t, fake)

	for locale, want := range map[string]string{
		"en": "request was not valid",
		"el": "αίτημα δεν ήταν έγκυρο",
		"de": "αίτημα δεν ήταν έγκυρο",
	} {
		t.Run(locale, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"message": "hi", "locale": locale, "cartId": "not-a-cart",
			})
			require.NoError(t, err)
			resp, err := http.Post(gw.URL+"/chat", "application/json",
				bytes.NewReader(raw))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			var payload map[string]string
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
			assert.Contains(t, payload["error"], want)
		})
	}
	assert.Zero(t, fake.calls.Load())
}
