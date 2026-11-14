// Package catalog answers what a provider's model costs and how much context it holds.
package catalog

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/wolfeidau/durable"
)

// lookupTimeout bounds a background lookup, so nothing waits on it at shutdown.
const lookupTimeout = 10 * time.Second

// Rates are USD per token.
type Rates struct {
	Prompt, Completion, CacheRead, CacheWrite float64
}

// Tier applies once the prompt reaches MinPromptTokens.
type Tier struct {
	MinPromptTokens int64
	Rates           Rates
}

type Price struct {
	Base  Rates
	Tiers []Tier
}

// Info is what a catalog knows about one model; zero fields are unknown.
type Info struct {
	ContextWindow int64
	Price         *Price
}

// Catalog is a provider's model list.
type Catalog interface {
	Lookup(ctx context.Context, model string) (Info, error)
}

// Estimate prices u at the tier with the highest threshold its prompt reaches.
func (p *Price) Estimate(u *durable.Usage) float64 {
	r := p.Base
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	var best int64 = -1
	for _, t := range p.Tiers {
		if t.MinPromptTokens <= prompt && t.MinPromptTokens > best {
			r, best = t.Rates, t.MinPromptTokens
		}
	}
	return r.Prompt*float64(u.InputTokens) + r.CacheRead*float64(u.CacheReadTokens) +
		r.CacheWrite*float64(u.CacheWriteTokens) + r.Completion*float64(u.OutputTokens)
}

// Pending is a lookup running in the background.
type Pending struct{ info atomic.Pointer[Info] }

// Start looks the model up without blocking, so opening a session never waits on the network.
// done, if set, runs once on the lookup goroutine.
func Start(ctx context.Context, c Catalog, model string, done func(Info, error)) *Pending {
	p := &Pending{}
	// A server's request context ends when the response is sent, long before the lookup should.
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		info, err := c.Lookup(ctx, model)
		if err == nil {
			p.info.Store(&info)
		}
		if done != nil {
			done(info, err)
		}
	}()
	return p
}

// Info never blocks, and is false until a lookup has succeeded.
func (p *Pending) Info() (Info, bool) {
	if i := p.info.Load(); i != nil {
		return *i, true
	}
	return Info{}, false
}
