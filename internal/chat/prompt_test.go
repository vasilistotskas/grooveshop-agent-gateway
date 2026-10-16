package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func testTenant() *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName:      "demostore",
		StoreName:       "Demo Store",
		DefaultLocale:   "el",
		DefaultCurrency: "EUR",
		Domain:          "shop.example.test",
	}
}

func TestSystemPromptWithoutCart(t *testing.T) {
	p := systemPrompt(testTenant(), "")
	assert.Contains(t, p, "Demo Store")
	assert.Contains(t, p, "shop.example.test")
	assert.Contains(t, p, "no active cart")
	assert.Contains(t, p, "NEVER happens in chat")
}

func TestSystemPromptIsTenantNeutral(t *testing.T) {
	tn := testTenant()
	tn.DefaultLocale = "de"
	p := systemPrompt(tn, "")
	assert.Contains(t, p, "the store's language (de)")
	assert.NotContains(t, p, "Greek")
	assert.NotContains(t, p, "Greece")
	assert.NotContains(t, p, "ACS")
	assert.NotContains(t, p, "BOX NOW")
}

// Reviews are a store feature: the assistant offers them only to a store
// that has them on, as only that store is served the review tool.
func TestSystemPromptOffersReviewsOnlyWithThem(t *testing.T) {
	tn := testTenant()
	assert.NotContains(t, systemPrompt(tn, ""), "reviews")

	tn.Features.ProductReviews = true
	assert.Contains(t, systemPrompt(tn, ""), "categories, reviews, carts")
}

// A failed tool is reported as one: the assistant had claimed a cart
// update that never reached the shopper's cart, then blamed a "cartId
// mix-up" no tool reported.
func TestSystemPromptReportsFailuresAsTheToolGaveThem(t *testing.T) {
	p := systemPrompt(testTenant(), "")
	assert.Contains(t, p, "never claim the action succeeded")
	assert.Contains(t, p, "never give a cause the tool did not")
}

func TestSystemPromptWithCart(t *testing.T) {
	p := systemPrompt(testTenant(), "cart-uuid-1")
	assert.Contains(t, p, "cart-uuid-1")
	assert.NotContains(t, p, "no active cart")
}

func TestLoadRejectsMalformedConversationID(t *testing.T) {
	s := NewStore(nil, 0, 10)
	_, err := s.Load(t.Context(), "demostore", "../evil")
	assert.ErrorIs(t, err, ErrInvalidConversation,
		"client input, answered 400 rather than 503")
}

func TestLoadEmptyIDStartsFresh(t *testing.T) {
	s := NewStore(nil, 0, 10)
	c, err := s.Load(t.Context(), "demostore", "")
	assert.NoError(t, err)
	assert.NotEmpty(t, c.ID)
	assert.Empty(t, c.Turns)
}
