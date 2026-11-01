package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/claude"
	"github.com/wolfeidau/durable/responses"
)

const (
	providerPinFile = "provider.json"

	providerAnthropic  = "anthropic"
	providerOpenRouter = "openrouter"

	protocolMessages  = "anthropic-messages"
	protocolResponses = "openai-responses"
)

// providerSpec says where a provider's requests go and which credential they use.
type providerSpec struct {
	// BaseURL is the root without /v1; empty uses the SDK default.
	BaseURL    string
	BaseURLEnv string
	KeyEnv     string
	Default    string
	Allowed    []string
}

var providers = map[string]providerSpec{
	providerAnthropic: {Default: protocolMessages, Allowed: []string{protocolMessages}},
	providerOpenRouter: {
		BaseURL:    "https://openrouter.ai/api",
		BaseURLEnv: "OPENROUTER_BASE_URL",
		KeyEnv:     "OPENROUTER_API_KEY",
		Default:    protocolResponses,
		Allowed:    []string{protocolResponses, protocolMessages},
	},
}

// route is pinned per session, because stored Raw content is specific to its protocol.
type route struct {
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	Model    string `json:"model,omitempty"`
}

// routeFlags are the user's explicit choices; empty means use the pin or default.
type routeFlags struct {
	Provider, Protocol, Model string
}

var legacyRoute = route{Provider: providerAnthropic, Protocol: protocolMessages}

func providerNames() string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func (r route) spec() (providerSpec, error) {
	s, ok := providers[r.Provider]
	if !ok {
		return providerSpec{}, fmt.Errorf("unknown provider %q (use one of: %s)", r.Provider, providerNames())
	}
	if !slices.Contains(s.Allowed, r.Protocol) {
		return providerSpec{}, fmt.Errorf("provider %s does not support protocol %q (use one of: %s)", r.Provider, r.Protocol, strings.Join(s.Allowed, ", "))
	}
	return s, nil
}

func newRoute(f routeFlags) (route, error) {
	r := route(f)
	if r.Provider == "" {
		r.Provider = providerAnthropic
	}
	s, ok := providers[r.Provider]
	if !ok {
		return route{}, fmt.Errorf("unknown provider %q (use one of: %s)", r.Provider, providerNames())
	}
	if r.Protocol == "" {
		r.Protocol = s.Default
	}
	if _, err := r.spec(); err != nil {
		return route{}, err
	}
	switch {
	case r.Provider == providerAnthropic:
		// The model isn't pinned for anthropic, as before.
		r.Model = ""
	case r.Model == "":
		return route{}, fmt.Errorf("--model is required for provider %s", r.Provider)
	}
	return r, nil
}

// matches rejects an explicit flag that differs from the session's pin.
func (r route) matches(f routeFlags) error {
	if _, err := r.spec(); err != nil {
		return fmt.Errorf("invalid pinned provider.json: %w", err)
	}
	if f.Provider != "" && f.Provider != r.Provider {
		return fmt.Errorf("this session uses provider %s, not %s; start a new --session to switch", r.Provider, f.Provider)
	}
	if f.Protocol != "" && f.Protocol != r.Protocol {
		return fmt.Errorf("this session uses protocol %s, not %s; start a new --session to switch", r.Protocol, f.Protocol)
	}
	// Reasoning items can be bound to the model that wrote them.
	if r.Provider != providerAnthropic && f.Model != "" && f.Model != r.Model {
		return fmt.Errorf("this session uses model %s, not %s; start a new --session to switch", r.Model, f.Model)
	}
	return nil
}

// pinRoute pins the route on first start. check runs on the final route, before any pin is written.
func pinRoute(dir string, existed bool, f routeFlags, check func(route) error) (route, bool, error) {
	cur, ok, err := readJSONPin[route](filepath.Join(dir, providerPinFile), "provider")
	if err != nil {
		return route{}, false, err
	}
	// A session that predates the pin is anthropic.
	if !ok && existed {
		cur, ok = legacyRoute, true
	}
	validate := func(r route) error {
		if err := r.matches(f); err != nil {
			return err
		}
		return check(r)
	}
	if ok {
		if err := validate(cur); err != nil {
			return route{}, false, err
		}
	}
	r, pinned, err := pinJSON(dir, providerPinFile, "provider", existed, legacyRoute, func() (route, error) {
		r, err := newRoute(f)
		if err != nil {
			return route{}, err
		}
		return r, check(r)
	})
	if err != nil {
		return route{}, false, err
	}
	// Another process pinned first, so its route wins.
	if pinned && !ok {
		if err := validate(r); err != nil {
			return route{}, false, err
		}
	}
	return r, pinned, nil
}

// resolveKey returns the provider's key from the flag token, then its environment variable.
func resolveKey(flagToken string, r route, s providerSpec) (string, error) {
	if flagToken != "" || s.KeyEnv == "" {
		return flagToken, nil
	}
	if k := os.Getenv(s.KeyEnv); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("no API key for provider %s: set %s, or pass --api-token or --api-token-from-1password", r.Provider, s.KeyEnv)
}

// modelArgs is what a protocol constructor needs beyond the route.
type modelArgs struct {
	Key       string
	System    string
	Effort    string
	SessionID string
	OnText    func(string)
	OnCompact func()
}

var protocolModels = map[string]func(r route, s providerSpec, a modelArgs) durable.Model{
	protocolMessages: func(r route, s providerSpec, a modelArgs) durable.Model {
		var opts []option.RequestOption
		switch {
		case s.BaseURL != "":
			// Gateways must never see Anthropic credentials from the environment, or the reverse.
			// Those defaults also carry the SDK's header timeout, so set an equivalent client.
			opts = []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithHTTPClient(responses.DefaultHTTPClient()), option.WithBaseURL(baseURL(s)), option.WithAuthToken(a.Key)}
		case a.Key != "":
			opts = []option.RequestOption{option.WithAPIKey(a.Key)}
		}
		return claude.New(anthropic.NewClient(opts...), claude.Config{
			Model:     r.Model,
			System:    a.System,
			Effort:    a.Effort,
			Gateway:   s.BaseURL != "",
			OnText:    a.OnText,
			OnCompact: a.OnCompact,
		})
	},
	protocolResponses: func(r route, s providerSpec, a modelArgs) durable.Model {
		return responses.New(responses.Config{
			BaseURL:   baseURL(s) + "/v1",
			APIKey:    a.Key,
			Model:     r.Model,
			System:    a.System,
			Effort:    a.Effort,
			SessionID: a.SessionID,
			OnText:    a.OnText,
		})
	},
}

func baseURL(s providerSpec) string {
	if v := os.Getenv(s.BaseURLEnv); s.BaseURLEnv != "" && v != "" {
		return strings.TrimRight(v, "/")
	}
	return s.BaseURL
}

// routeLine makes the route visible, and warns about the one combination known to defeat prompt caching.
func routeLine(r route) (line, warning string) {
	model := r.Model
	if model == "" {
		model = claude.DefaultModel
	}
	line = fmt.Sprintf("[model] %s via %s: %s", r.Provider, r.Protocol, model)
	if r.Protocol == protocolResponses && strings.HasPrefix(model, "anthropic/") {
		warning = fmt.Sprintf("[model] warning: %s on %s misses Anthropic prompt caching and can cost several times more; start a new --session with --protocol %s", model, protocolResponses, protocolMessages)
	}
	return line, warning
}
