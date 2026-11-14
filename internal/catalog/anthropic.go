package catalog

import (
	"context"
	"regexp"
)

var minorVersion = regexp.MustCompile(`-(\d+)-(\d+)$`)

// AnthropicDirect has no model list of its own, so it borrows another catalog's entries for Anthropic's models.
type AnthropicDirect struct {
	Prices Catalog
	// DefaultModel stands in for an empty model name.
	DefaultModel string
}

// Lookup maps a direct Anthropic name to its OpenRouter id, such as claude-sonnet-5-5 to anthropic/claude-sonnet-5.5.
func (a AnthropicDirect) Lookup(ctx context.Context, model string) (Info, error) {
	if model == "" {
		model = a.DefaultModel
	}
	return a.Prices.Lookup(ctx, "anthropic/"+minorVersion.ReplaceAllString(model, "-$1.$2"))
}
