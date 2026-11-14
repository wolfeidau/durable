package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/wolfeidau/durable/claude"
)

const (
	modelsTimeout = 5 * time.Second
	// The full model list is several MB.
	modelsMaxBytes = 32 << 20
	// The list changes rarely, and a server opens a session per request.
	modelsTTL = time.Hour
)

// rates are USD per token.
type rates struct {
	Prompt, Completion, CacheRead, CacheWrite, CacheWrite1h float64
}

// priceTier applies once the prompt reaches MinPromptTokens.
type priceTier struct {
	MinPromptTokens int64
	Rates           rates
}

type modelPrice struct {
	Base      rates
	Overrides []priceTier
}

// modelInfo is what the provider's model list says about one model; zero fields are unknown.
type modelInfo struct {
	ContextWindow int64
	Price         *modelPrice
}

// usd reads the list's string prices, and a plain number too.
type usd float64

func (u *usd) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var f float64
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		*u = usd(f)
		return nil
	}
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*u = usd(f)
	return nil
}

type wireRates struct {
	Prompt       usd `json:"prompt"`
	Completion   usd `json:"completion"`
	CacheRead    usd `json:"input_cache_read"`
	CacheWrite   usd `json:"input_cache_write"`
	CacheWrite1h usd `json:"input_cache_write_1h"`
}

func (w wireRates) rates() rates {
	return rates{float64(w.Prompt), float64(w.Completion), float64(w.CacheRead), float64(w.CacheWrite), float64(w.CacheWrite1h)}
}

type wirePricing struct {
	wireRates
	Overrides []struct {
		wireRates
		MinPromptTokens int64 `json:"min_prompt_tokens"`
	} `json:"overrides"`
}

// price is nil when the model has no usable price, such as the router's -1 placeholders or an empty block.
func (w wirePricing) price() *modelPrice {
	if w.Prompt < 0 || w.Completion < 0 || (w.Prompt == 0 && w.Completion == 0) {
		return nil
	}
	p := &modelPrice{Base: w.rates()}
	for _, o := range w.Overrides {
		p.Overrides = append(p.Overrides, priceTier{MinPromptTokens: o.MinPromptTokens, Rates: o.rates()})
	}
	return p
}

// modelList is the parsed list for one base URL.
type modelList struct {
	at     time.Time
	models map[string]modelInfo
}

// modelCache is shared by every session in the process, because the list is several MB.
var modelCache = struct {
	mu    sync.Mutex
	lists map[string]modelList
	now   func() time.Time
}{lists: map[string]modelList{}, now: time.Now}

// fetchModelInfo reads the model's window and price from the provider's model list, cached for an hour.
// Errors are not cached and never include the key.
func fetchModelInfo(ctx context.Context, client *http.Client, baseURL, key, model string) (modelInfo, error) {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()
	list, ok := modelCache.lists[baseURL]
	if !ok || modelCache.now().Sub(list.at) >= modelsTTL {
		models, err := fetchModelList(ctx, client, baseURL, key)
		if err != nil {
			return modelInfo{}, err
		}
		list = modelList{at: modelCache.now(), models: models}
		modelCache.lists[baseURL] = list
	}
	info, ok := list.models[model]
	if !ok {
		return modelInfo{}, errors.New("model " + model + " is not in the model list")
	}
	return info, nil
}

func fetchModelList(ctx context.Context, client *http.Client, baseURL, key string) (map[string]modelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build models request: %w", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list returned HTTP %d", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID            string          `json:"id"`
			ContextLength int64           `json:"context_length"`
			Pricing       json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelsMaxBytes)).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode model list: %w", err)
	}
	out := make(map[string]modelInfo, len(list.Data))
	for _, m := range list.Data {
		info := modelInfo{ContextWindow: max(m.ContextLength, 0)}
		var p wirePricing
		// A malformed price leaves the model priced as unknown rather than failing the list.
		if len(m.Pricing) > 0 && json.Unmarshal(m.Pricing, &p) == nil {
			info.Price = p.price()
		}
		out[m.ID] = info
	}
	return out, nil
}

var anthropicMinorVersion = regexp.MustCompile(`-(\d+)-(\d+)$`)

// openRouterAnthropicID maps a direct Anthropic model name to its OpenRouter id, such as claude-sonnet-5-5 to anthropic/claude-sonnet-5.5.
func openRouterAnthropicID(model string) string {
	if model == "" {
		model = claude.DefaultModel
	}
	return "anthropic/" + anthropicMinorVersion.ReplaceAllString(model, "-$1.$2")
}

// loadModelInfo finds the session model's window and price. A direct Anthropic session asks OpenRouter's public list, with no key.
func loadModelInfo(ctx context.Context, rt route, spec providerSpec, key string) (modelInfo, error) {
	if rt.Provider == providerAnthropic {
		return fetchModelInfo(ctx, &http.Client{}, baseURL(providers[providerOpenRouter]), "", openRouterAnthropicID(rt.Model))
	}
	return fetchModelInfo(ctx, &http.Client{}, baseURL(spec), key, rt.Model)
}
