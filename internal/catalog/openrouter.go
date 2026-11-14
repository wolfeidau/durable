package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	fetchTimeout = 5 * time.Second
	// The full model list is several MB.
	maxListBytes = 32 << 20
	// The list changes rarely, and a server opens a session per request.
	listTTL = time.Hour
	// A host that can't reach the list would otherwise wait out fetchTimeout on every session open.
	retryAfter = time.Minute
)

// OpenRouter reads OpenRouter's model list; Key may be empty, as the list is public.
type OpenRouter struct {
	BaseURL string
	Key     string
	Client  *http.Client
}

// list is one fetch; done closes once models and err are set.
type list struct {
	done   chan struct{}
	at     time.Time
	models map[string]Info
	err    error
}

// cache is shared by every session in the process, because the list is several MB.
var cache = struct {
	mu    sync.Mutex
	lists map[string]*list
	now   func() time.Time
}{lists: map[string]*list{}, now: time.Now}

// Lookup serves the list from a per-URL cache that callers share, and never holds a lock across the network.
func (o OpenRouter) Lookup(ctx context.Context, model string) (Info, error) {
	l, err := o.list(ctx)
	if err != nil {
		return Info{}, err
	}
	info, ok := l[model]
	if !ok {
		return Info{}, errors.New("model " + model + " is not in the model list")
	}
	return info, nil
}

// cacheKey separates keyless from keyed fetches, because a gateway can answer them differently; the key itself is never stored.
func (o OpenRouter) cacheKey() string {
	return o.BaseURL + "|" + strconv.FormatBool(o.Key != "")
}

func (o OpenRouter) list(ctx context.Context) (map[string]Info, error) {
	key := o.cacheKey()
	cache.mu.Lock()
	l := cache.lists[key]
	if l != nil {
		select {
		case <-l.done:
			ttl := listTTL
			if l.err != nil {
				ttl = retryAfter
			}
			if cache.now().Sub(l.at) >= ttl {
				l = nil
			}
		default:
		}
	}
	fetch := l == nil
	if fetch {
		l = &list{done: make(chan struct{})}
		cache.lists[key] = l
	}
	cache.mu.Unlock()

	if fetch {
		l.models, l.err = o.fetch(ctx)
		cache.mu.Lock()
		l.at = cache.now()
		cache.mu.Unlock()
		close(l.done)
	} else {
		select {
		case <-l.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("failed to list models: %w", ctx.Err())
		}
	}
	return l.models, l.err
}

func (o OpenRouter) fetch(ctx context.Context) (map[string]Info, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build models request: %w", err)
	}
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID            string          `json:"id"`
			ContextLength int64           `json:"context_length"`
			Pricing       json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to decode model list: %w", err)
	}
	out := make(map[string]Info, len(body.Data))
	for _, m := range body.Data {
		info := Info{ContextWindow: max(m.ContextLength, 0)}
		var p wirePricing
		// A malformed price leaves the model priced as unknown rather than failing the list.
		if len(m.Pricing) > 0 && json.Unmarshal(m.Pricing, &p) == nil {
			info.Price = p.price()
		}
		out[m.ID] = info
	}
	return out, nil
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
	Prompt     usd `json:"prompt"`
	Completion usd `json:"completion"`
	CacheRead  usd `json:"input_cache_read"`
	CacheWrite usd `json:"input_cache_write"`
}

func (w wireRates) rates() Rates {
	return Rates{float64(w.Prompt), float64(w.Completion), float64(w.CacheRead), float64(w.CacheWrite)}
}

type wirePricing struct {
	wireRates
	Overrides []struct {
		wireRates
		MinPromptTokens int64 `json:"min_prompt_tokens"`
	} `json:"overrides"`
}

// price is nil when the model has no usable price, such as the router's -1 placeholders or an empty block.
func (w wirePricing) price() *Price {
	if w.Prompt < 0 || w.Completion < 0 || (w.Prompt == 0 && w.Completion == 0) {
		return nil
	}
	p := &Price{Base: w.rates()}
	for _, o := range w.Overrides {
		p.Tiers = append(p.Tiers, Tier{MinPromptTokens: o.MinPromptTokens, Rates: o.rates()})
	}
	return p
}
