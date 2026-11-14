// Package catalog answers what a provider's model costs and how much context it holds.
package catalog

import (
	"context"
	"sync"
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
type Pending struct {
	info atomic.Pointer[Info]

	c     Catalog
	model string
	ctx   context.Context
	done  func(Info, error)

	mu       sync.Mutex
	inFlight bool
	failedAt time.Time
}

// Start looks the model up without blocking, so opening a session never waits on the network.
// done, if set, runs on the lookup goroutine for the first attempt's outcome, and after that only
// when a retry succeeds, so a long outage doesn't repeat the notice.
func Start(ctx context.Context, c Catalog, model string, done func(Info, error)) *Pending {
	// A server's request context ends when the response is sent, long before the lookup should.
	p := &Pending{c: c, model: model, ctx: context.WithoutCancel(ctx), done: done, inFlight: true}
	go p.run(true)
	return p
}

func (p *Pending) run(first bool) {
	ctx, cancel := context.WithTimeout(p.ctx, lookupTimeout)
	defer cancel()
	info, err := p.c.Lookup(ctx, p.model)
	if err == nil {
		p.info.Store(&info)
	}
	p.mu.Lock()
	p.inFlight = false
	if err != nil {
		p.failedAt = cache.now()
	}
	p.mu.Unlock()
	if p.done != nil && (first || err == nil) {
		p.done(info, err)
	}
}

// Info never blocks, and is false until a lookup has succeeded. A failed lookup is retried in the
// background on a call at least retryAfter after the last failure.
func (p *Pending) Info() (Info, bool) {
	if i := p.info.Load(); i != nil {
		return *i, true
	}
	p.mu.Lock()
	// Re-check under the lock: a lookup that just succeeded must not start another.
	retry := p.info.Load() == nil && !p.inFlight && cache.now().Sub(p.failedAt) >= retryAfter
	if retry {
		p.inFlight = true
	}
	p.mu.Unlock()
	if retry {
		go p.run(false)
	}
	return Info{}, false
}
