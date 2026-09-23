// Package media builds public asset URLs from the single config-driven
// template. The template carries every media-stream path segment (size,
// fit, quality, format); code only substitutes placeholders — environment
// differences are config values, never conditionals.
package media

import (
	"net/url"
	"strings"
)

// Host picks the media origin for a tenant: its own assets host when it
// has opted into white-label asset URLs, otherwise the platform origin.
//
// Deriving the host from the tenant's storefront domain instead (the old
// "assets.{domain}" template) produced a hostname that standard
// onboarding never creates — the infra repo's docs/tenant-onboarding.md
// states that assets hosts are NOT provisioned per tenant and every
// tenant shares the platform origin. Django and Nuxt both honour that fallback; the
// gateway emitted NXDOMAIN image URLs into every feed and agent
// response, which Meta and TikTok reject silently because nothing here
// ever fetches what it emits.
func Host(tenantAssetsDomain, platformHost string) string {
	if tenantAssetsDomain != "" {
		return tenantAssetsDomain
	}
	return platformHost
}

// ImageURL expands the template's {assets_host}, {schema} and {path}
// placeholders. An empty source path — or an unresolved media host —
// yields an empty URL, so a missing configuration drops the image
// rather than publishing an unreachable one.
//
// The path is Django's raw storage name, so each segment is
// percent-encoded here: a Greek filename is not a valid URI, and feed
// crawlers and UCP platforms (image_url is format: uri) reject it.
func ImageURL(template, assetsHost, schema, path string) string {
	if path == "" || template == "" || assetsHost == "" {
		return ""
	}
	return strings.NewReplacer(
		"{assets_host}", assetsHost,
		"{schema}", schema,
		"{path}", escapePath(path),
	).Replace(template)
}

func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
