package checkout

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// RegionOption is a delivery region a buyer can pick for the session's
// country. Code is what the order names it by.
type RegionOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// ResolveRegion settles the session's delivery region from what the caller
// named: a region code or name, in any language the store has it in,
// whatever its case or accents. Django refuses an order without a region
// when the country has any, so the session keeps the country's regions and
// stays incomplete until one is chosen. Naming nothing, or a region the
// country does not have, leaves it unchosen, and Missing lists the choices.
func ResolveRegion(
	ctx context.Context, dj *django.Client, t *tenant.Tenant, s *Session,
	named string,
) error {
	s.Fulfillment.RegionID = ""
	s.RegionOptions = nil
	// Django knows a country only by its upper-case code: in any other case
	// it has no regions, and refuses the order at the end.
	s.Fulfillment.CountryCode = strings.ToUpper(s.Fulfillment.CountryCode)
	if s.Fulfillment.CountryCode == "" {
		return nil
	}
	regions, err := dj.Regions(
		ctx, t.Domain, t.DefaultLocale, s.Fulfillment.CountryCode)
	if err != nil {
		return fmt.Errorf("checkout: regions: %w", err)
	}
	want := foldRegion(named)
	for _, r := range regions {
		s.RegionOptions = append(s.RegionOptions, RegionOption{
			Code: r.Alpha,
			Name: django.Localized(r.Translations, t.DefaultLocale).Name,
		})
		if s.Fulfillment.RegionID == "" && want != "" &&
			regionNamed(r, want) {
			s.Fulfillment.RegionID = r.Alpha
		}
	}
	return nil
}

func regionNamed(r django.Region, folded string) bool {
	if foldRegion(r.Alpha) == folded {
		return true
	}
	for _, tr := range r.Translations {
		if foldRegion(tr.Name) == folded {
			return true
		}
	}
	return false
}

// foldRegion reduces a region code or name to what a buyer means by it:
// "ΑΤΤΙΚΗ" and "Αττική" are one region.
func foldRegion(s string) string {
	s = strings.TrimSpace(s)
	stripped, _, err := transform.String(transform.Chain(
		norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC,
	), s)
	if err != nil {
		stripped = s
	}
	return cases.Fold().String(stripped)
}

// regionMissing reports whether the country has regions and none is
// chosen: Django would refuse the order.
func (s *Session) regionMissing() bool {
	return len(s.RegionOptions) > 0 && s.Fulfillment.RegionID == ""
}

// RegionChoices lists the regions the caller can name, or "" while none
// is needed.
func (s *Session) RegionChoices() string {
	if !s.regionMissing() {
		return ""
	}
	choices := make([]string, 0, len(s.RegionOptions))
	for _, r := range s.RegionOptions {
		choices = append(choices, r.Code+" ("+r.Name+")")
	}
	return strings.Join(choices, ", ")
}
