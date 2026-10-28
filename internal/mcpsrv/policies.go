package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type StorePoliciesIn struct{}

type StorePoliciesOut struct {
	ReturnWindowDays int    `json:"returnWindowDays,omitempty" jsonschema:"days after delivery a product can be returned; absent when the store has not stated it"`
	ReturnFees       string `json:"returnFees,omitempty" jsonschema:"who pays to send a return back: free (the store) or customer; absent when the store has not stated it"`
	ReturnPolicyURL  string `json:"returnPolicyUrl,omitempty" jsonschema:"the store's return-policy page; absent when the store publishes none"`
}

// getStorePolicies states the store's return terms as the storefront
// does (its Organization node and return-policy page): the window and
// fees from its public settings, the page only once it is published.
func (h *handlers) getStorePolicies(
	ctx context.Context, _ *mcp.CallToolRequest, _ StorePoliciesIn,
) (*mcp.CallToolResult, StorePoliciesOut, error) {
	var out StorePoliciesOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)

	settings, err := h.deps.Django.PublicSettings(ctx, t.Domain, lang)
	if err != nil {
		return nil, out, upstreamErr(err, "store policies are unavailable")
	}
	if out.ReturnWindowDays, err = returnWindowDays(settings); err != nil {
		return nil, out, err
	}
	if out.ReturnFees, err = returnFees(settings); err != nil {
		return nil, out, err
	}

	_, err = h.deps.Django.ContentPage(
		ctx, t.Domain, lang, django.ContentPageReturnPolicy)
	switch {
	case err == nil:
		out.ReturnPolicyURL = h.linksFor(ctx, t).ReturnPolicy()
	case !errors.Is(err, django.ErrNotFound):
		return nil, out, upstreamErr(err, "store policies are unavailable")
	}

	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: policiesSummary(out)},
	}}, out, nil
}

// returnWindowDays reads RETURN_WINDOW_DAYS: 0 when the store has not
// stated it. Django validates it as a whole number of days, so anything
// else is a broken contract, not a policy.
func returnWindowDays(s *django.PublicSettings) (int, error) {
	raw, ok := s.Settings[django.SettingReturnWindowDays]
	if !ok {
		return 0, unreadableTerms(django.SettingReturnWindowDays)
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 0 {
		return 0, unreadableTerms(django.SettingReturnWindowDays)
	}
	return days, nil
}

// returnFees reads RETURN_FEES: "" when the store has not stated it.
func returnFees(s *django.PublicSettings) (string, error) {
	raw, ok := s.Settings[django.SettingReturnFees]
	if !ok {
		return "", unreadableTerms(django.SettingReturnFees)
	}
	switch raw {
	case "", django.ReturnFeesFree, django.ReturnFeesCustomer:
		return raw, nil
	}
	return "", unreadableTerms(django.SettingReturnFees)
}

// unreadableTerms reports a return setting Django did not serve as it
// declares it.
func unreadableTerms(name string) error {
	return &ToolError{Code: codeUnavailable, Hint: "the store's " + name +
		" could not be read; tell the shopper its return terms cannot " +
		"be read right now, and never state terms of your own"}
}

func policiesSummary(out StorePoliciesOut) string {
	var b strings.Builder
	if out.ReturnWindowDays > 0 {
		fmt.Fprintf(&b, "Returns are accepted within %d days of "+
			"delivery.", out.ReturnWindowDays)
	} else {
		b.WriteString("The store has not stated a return window.")
	}
	switch out.ReturnFees {
	case django.ReturnFeesFree:
		b.WriteString(" Return shipping is free for the shopper.")
	case django.ReturnFeesCustomer:
		b.WriteString(" The shopper pays the return shipping.")
	default:
		b.WriteString(" The store has not stated who pays return " +
			"shipping.")
	}
	if out.ReturnPolicyURL != "" {
		b.WriteString(" Full return policy: " + out.ReturnPolicyURL)
	} else {
		b.WriteString(" The store publishes no return policy page.")
	}
	return b.String()
}
