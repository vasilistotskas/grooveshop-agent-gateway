// Package media builds public asset URLs from the single config-driven
// template. The template carries every media-stream path segment (size,
// fit, quality, format); code only substitutes placeholders — environment
// differences are config values, never conditionals.
package media

import "strings"

// ImageURL expands the template's {domain}, {schema} and {path}
// placeholders. An empty source path yields an empty URL — callers decide
// how to represent "no image".
func ImageURL(template, domain, schema, path string) string {
	if path == "" || template == "" {
		return ""
	}
	return strings.NewReplacer(
		"{domain}", domain,
		"{schema}", schema,
		"{path}", path,
	).Replace(template)
}
