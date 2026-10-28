package django

import (
	"context"
	"net/url"
)

// PublicSettings are the store values Django serves anonymously, keyed
// by setting name: every PUBLIC_SETTING_KEYS entry (core/api/views.py),
// with the store's value or the platform's declared one. Values are
// strings in Django's settings encoding.
type PublicSettings struct {
	Settings map[string]string `json:"settings"`
}

// Store settings the gateway reads from PublicSettings. Both are the
// return terms the store's return-policy page states.
const (
	// SettingReturnWindowDays: whole days after delivery a product can
	// be returned; "0" is not stated.
	SettingReturnWindowDays = "RETURN_WINDOW_DAYS"
	// SettingReturnFees: who pays to send a return back, one of
	// ReturnFeesFree, ReturnFeesCustomer; "" is not stated.
	SettingReturnFees = "RETURN_FEES"
)

// RETURN_FEES values (tenant/validators.py RETURN_FEES_CHOICES).
const (
	ReturnFeesFree     = "free"
	ReturnFeesCustomer = "customer"
)

// PublicSettings reads the store's public settings, as the storefront
// does once per render.
func (c *Client) PublicSettings(
	ctx context.Context, host, lang string,
) (*PublicSettings, error) {
	var out PublicSettings
	err := c.get(ctx, request{
		path:     "/settings/public",
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ContentPage is a store page the merchant writes (FAQ, the legal
// documents). Django serves a page anonymously only once the store has
// published it, so ErrNotFound means the store publishes none at slug.
type ContentPage struct {
	Slug string `json:"slug"`
}

// ContentPageReturnPolicy is the return policy's slug
// (page_config/legal_documents.py RETURN_POLICY_SLUG).
const ContentPageReturnPolicy = "return-policy"

// ContentPage fetches the store's published page at slug.
func (c *Client) ContentPage(
	ctx context.Context, host, lang, slug string,
) (*ContentPage, error) {
	var out ContentPage
	err := c.get(ctx, request{
		path:     "/content-page/{slug}",
		rawPath:  "/content-page/" + url.PathEscape(slug),
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
