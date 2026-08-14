package feeds

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ACP feed types mirror testdata/schemas/acp/2026-04-17/schema.feed.json.
// Prices are integer ISO 4217 minor units. Each catalog row becomes one
// Product with a single self-Variant: variant grouping across the streamed
// catalog would require full buffering, and the RSS feeds already carry
// item_group_id for the platforms that use it.

type acpPrice struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// acpDescription is the spec's structured description (plain/html/markdown,
// at least one).
type acpDescription struct {
	Plain string `json:"plain"`
}

type acpAvailability struct {
	Available bool   `json:"available"`
	Status    string `json:"status"`
}

type acpMedia struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type acpVariant struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Description  *acpDescription  `json:"description,omitempty"`
	URL          string           `json:"url,omitempty"`
	Price        *acpPrice        `json:"price,omitempty"`
	ListPrice    *acpPrice        `json:"list_price,omitempty"`
	Availability *acpAvailability `json:"availability,omitempty"`
	Condition    []string         `json:"condition,omitempty"`
	Media        []acpMedia       `json:"media,omitempty"`
}

type acpProduct struct {
	ID          string          `json:"id"`
	Title       string          `json:"title,omitempty"`
	Description *acpDescription `json:"description,omitempty"`
	URL         string          `json:"url,omitempty"`
	Media       []acpMedia      `json:"media,omitempty"`
	Variants    []acpVariant    `json:"variants"`
}

type acpFeed struct {
	Products []acpProduct `json:"products"`
}

type acpWriter struct {
	feed acpFeed
}

func newACPWriter() *acpWriter {
	return &acpWriter{feed: acpFeed{Products: []acpProduct{}}}
}

func (w *acpWriter) Item(it *feedItem, finalPrice, regularPrice int64) {
	id := fmt.Sprintf("%d", it.ID)
	status := "out_of_stock"
	if it.InStock {
		status = "in_stock"
	}
	media := []acpMedia{{Type: "image", URL: it.ImageLink}}
	var desc *acpDescription
	if it.Description != "" {
		desc = &acpDescription{Plain: it.Description}
	}
	w.feed.Products = append(w.feed.Products, acpProduct{
		ID:          id,
		Title:       it.Title,
		Description: desc,
		URL:         it.Link,
		Media:       media,
		Variants: []acpVariant{{
			ID:    id,
			Title: it.Title,
			URL:   it.Link,
			Price: &acpPrice{
				Amount: finalPrice, Currency: it.Currency,
			},
			ListPrice: &acpPrice{
				Amount: regularPrice, Currency: it.Currency,
			},
			Availability: &acpAvailability{
				Available: it.InStock, Status: status,
			},
			Condition: []string{"new"},
			Media:     media,
		}},
	})
}

func (w *acpWriter) Bytes() ([]byte, error) {
	return json.MarshalIndent(w.feed, "", " ")
}

// minorUnits converts a dot-decimal amount to ISO 4217 minor units without
// float rounding ("464.68" -> 46468).
func minorUnits(decimal string) (int64, error) {
	whole, frac, _ := strings.Cut(decimal, ".")
	if whole == "" {
		whole = "0"
	}
	switch len(frac) {
	case 0:
		frac = "00"
	case 1:
		frac += "0"
	case 2:
	default:
		frac = frac[:2]
	}
	var n int64
	if _, err := fmt.Sscanf(whole+frac, "%d", &n); err != nil {
		return 0, fmt.Errorf("feeds: bad amount %q: %w", decimal, err)
	}
	return n, nil
}
