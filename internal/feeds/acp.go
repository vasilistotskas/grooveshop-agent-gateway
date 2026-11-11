package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// ACP feed types mirror testdata/schemas/acp/2026-04-17/schema.feed.json.
// Prices are integer ISO 4217 minor units. Each catalog row becomes one
// Product with a single self-Variant: the RSS feeds carry item_group_id
// for the platforms that use it.

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

// acpWriter writes the feed to out one product at a time, so the
// document is never held whole. A product is encoded as encoding/json
// encodes an element of a slice, so the document is byte for byte the
// one marshalling {"products": [...]} at once gives.
type acpWriter struct {
	out    io.Writer
	listed bool
}

// newACPWriter writes the document's head to out.
func newACPWriter(out io.Writer) (*acpWriter, error) {
	if _, err := io.WriteString(out, `{"products":[`); err != nil {
		return nil, err
	}
	return &acpWriter{out: out}, nil
}

func (w *acpWriter) Item(it *feedItem) error {
	row, err := json.Marshal(acpProductOf(it))
	if err != nil {
		return fmt.Errorf("feeds: acp encode: %w", err)
	}
	if w.listed {
		row = append([]byte{','}, row...)
	}
	w.listed = true
	_, err = w.out.Write(row)
	return err
}

// Close writes the document's end. It does not close out.
func (w *acpWriter) Close() error {
	_, err := io.WriteString(w.out, "]}")
	return err
}

func acpProductOf(it *feedItem) acpProduct {
	id := strconv.FormatInt(it.ID, 10)
	status := "out_of_stock"
	if it.InStock {
		status = "in_stock"
	}
	media := []acpMedia{{Type: "image", URL: it.ImageLink}}
	var desc *acpDescription
	if it.Description != "" {
		desc = &acpDescription{Plain: it.Description}
	}
	// list_price is the spec's pre-discount reference: only an announced
	// reduction's prior price may stand there.
	var listPrice *acpPrice
	if it.PriorMinor > 0 {
		listPrice = &acpPrice{Amount: it.PriorMinor, Currency: it.Currency}
	}
	return acpProduct{
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
				Amount: it.EffectiveMinor, Currency: it.Currency,
			},
			ListPrice: listPrice,
			Availability: &acpAvailability{
				Available: it.InStock, Status: status,
			},
			Condition: []string{"new"},
			Media:     media,
		}},
	}
}
