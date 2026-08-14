// Package acp implements the OpenAI Agentic Commerce Protocol REST surface
// (spec 2026-04-17): merchant-hosted checkout sessions under /acp/*,
// backed by the same protocol-neutral checkout sessions as UCP. Delegated
// tokenized payment (Stripe SPT) ships behind the per-tenant flag; until
// then cash on delivery completes agentically and card buyers hand off via
// continue_url.
package acp

import "encoding/json"

// Version is the implemented ACP specification version.
const Version = "2026-04-17"

// Wire types mirror testdata/schemas/acp/2026-04-17/schema.agentic_checkout
// (responses are additionalProperties:false — emit only known members;
// amounts are ISO 4217 minor units).

type Protocol struct {
	Version string `json:"version"`
}

type Capabilities struct {
	Payment *Payment `json:"payment,omitempty"`
}

type Payment struct {
	Handlers []PaymentHandler `json:"handlers"`
}

type PaymentHandler struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	PSP  string `json:"psp,omitempty"`
}

type Buyer struct {
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	Email       string `json:"email,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
}

type Address struct {
	Name       string `json:"name"`
	LineOne    string `json:"line_one"`
	LineTwo    string `json:"line_two,omitempty"`
	City       string `json:"city"`
	State      string `json:"state"`
	Country    string `json:"country"`
	PostalCode string `json:"postal_code"`
}

type FulfillmentDetails struct {
	Name        string   `json:"name,omitempty"`
	PhoneNumber string   `json:"phone_number,omitempty"`
	Email       string   `json:"email,omitempty"`
	Address     *Address `json:"address,omitempty"`
}

type Item struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	UnitAmount int64  `json:"unit_amount,omitempty"`
}

type Total struct {
	Type        string `json:"type"`
	DisplayText string `json:"display_text"`
	Amount      int64  `json:"amount"`
}

type LineItem struct {
	ID       string   `json:"id"`
	Item     Item     `json:"item"`
	Quantity int      `json:"quantity"`
	Name     string   `json:"name,omitempty"`
	Images   []string `json:"images,omitempty"`
	Totals   []Total  `json:"totals"`
}

type FulfillmentOption struct {
	Type   string  `json:"type"`
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Totals []Total `json:"totals"`
}

type SelectedFulfillmentOption struct {
	Type     string   `json:"type"`
	OptionID string   `json:"option_id"`
	ItemIDs  []string `json:"item_ids"`
}

// Message serializes as MessageInfo or MessageError depending on Type.
type Message struct {
	Type        string `json:"type"`
	Code        string `json:"code,omitempty"`
	Param       string `json:"param,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

type Link struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}

type Order struct {
	ID                string  `json:"id"`
	CheckoutSessionID string  `json:"checkout_session_id"`
	OrderNumber       string  `json:"order_number,omitempty"`
	PermalinkURL      string  `json:"permalink_url"`
	Totals            []Total `json:"totals,omitempty"`
}

// Session is the checkout session response (CheckoutSession, and
// CheckoutSessionWithOrder once Order is set).
type Session struct {
	ID                         string                      `json:"id"`
	Protocol                   Protocol                    `json:"protocol"`
	Capabilities               Capabilities                `json:"capabilities"`
	Buyer                      *Buyer                      `json:"buyer,omitempty"`
	Status                     string                      `json:"status"`
	Currency                   string                      `json:"currency"`
	LineItems                  []LineItem                  `json:"line_items"`
	FulfillmentDetails         *FulfillmentDetails         `json:"fulfillment_details,omitempty"`
	FulfillmentOptions         []FulfillmentOption         `json:"fulfillment_options"`
	SelectedFulfillmentOptions []SelectedFulfillmentOption `json:"selected_fulfillment_options,omitempty"`
	Totals                     []Total                     `json:"totals"`
	Messages                   []Message                   `json:"messages"`
	Links                      []Link                      `json:"links"`
	ContinueURL                string                      `json:"continue_url,omitempty"`
	Order                      *Order                      `json:"order,omitempty"`
}

// Error is the protocol-level 4xx/5xx body.
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

// Request bodies. Unknown members are ignored on decode.

type ItemRef struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
}

type CreateRequest struct {
	Buyer              *Buyer              `json:"buyer"`
	LineItems          []ItemRef           `json:"line_items"`
	Currency           string              `json:"currency"`
	FulfillmentDetails *FulfillmentDetails `json:"fulfillment_details"`
	Capabilities       json.RawMessage     `json:"capabilities"`
}

type UpdateRequest struct {
	Buyer                      *Buyer                      `json:"buyer"`
	LineItems                  []ItemRef                   `json:"line_items"`
	FulfillmentDetails         *FulfillmentDetails         `json:"fulfillment_details"`
	SelectedFulfillmentOptions []SelectedFulfillmentOption `json:"selected_fulfillment_options"`
}

type PaymentData struct {
	HandlerID  string          `json:"handler_id"`
	Instrument json.RawMessage `json:"instrument"`
}

type CompleteRequest struct {
	Buyer       *Buyer       `json:"buyer"`
	PaymentData *PaymentData `json:"payment_data"`
}
