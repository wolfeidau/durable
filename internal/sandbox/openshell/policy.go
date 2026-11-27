package openshell

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/ai-openshell/sdk-go/openshell/v1/types"

	"github.com/wolfeidau/durable/internal/sandbox"
)

// sandboxPolicy lets the worker binary, which runs the MCP bridge, reach the egress endpoints, and the repository
// clients reach only what access allows. Without a policy the sandbox has no egress at all. Giving one drops the
// gateway's default Landlock setting but keeps its filesystem and process defaults, so only Landlock is restated.
func sandboxPolicy(egress []sandbox.Endpoint, access []sandbox.GitAccess, grants []sandbox.Grant) *types.SandboxPolicy {
	if len(egress) == 0 && len(access) == 0 && len(grants) == 0 {
		return nil
	}
	rules := map[string]types.NetworkPolicyRule{}
	if len(egress) > 0 {
		endpoints := make([]types.PolicyNetworkEndpoint, 0, len(egress))
		for _, e := range egress {
			endpoints = append(endpoints, types.PolicyNetworkEndpoint{Host: e.Host, Port: e.Port})
		}
		rules[egressRule] = types.NetworkPolicyRule{Name: egressRule, Endpoints: endpoints, Binaries: []types.PolicyNetworkBinary{{Path: WorkerBinary}}}
	}
	if len(access) > 0 {
		rules[gitRule] = gitPolicyRule(access)
	}
	bindings := gitBindings(access)
	for _, g := range grants {
		rules[grantRulePrefix+g.Name] = grantPolicyRule(g, bindings)
	}
	return &types.SandboxPolicy{
		Landlock:        &types.LandlockPolicy{Compatibility: "best_effort"},
		NetworkPolicies: rules,
	}
}

// gitBindings maps each credentialed git endpoint to its provider.
func gitBindings(access []sandbox.GitAccess) map[sandbox.Endpoint]string {
	bindings := map[sandbox.Endpoint]string{}
	for _, a := range access {
		if a.Provider != "" {
			bindings[a.Endpoint] = a.Provider
		}
	}
	return bindings
}

// grantPolicyRule takes the git binding on a git host:port, because the gateway refuses overlapping endpoints whose bindings differ.
func grantPolicyRule(g sandbox.Grant, bindings map[sandbox.Endpoint]string) types.NetworkPolicyRule {
	name := grantRulePrefix + g.Name
	rule := types.NetworkPolicyRule{Name: name}
	for _, b := range g.Binaries {
		rule.Binaries = append(rule.Binaries, types.PolicyNetworkBinary{Path: b})
	}
	for _, e := range g.Endpoints {
		ep := types.PolicyNetworkEndpoint{Host: e.Host, Port: e.Port, Path: e.Path}
		switch {
		case len(e.GraphQL) > 0:
			ep.Protocol, ep.Enforcement = "graphql", types.NetworkEnforcementModeEnforce
			for _, r := range e.GraphQL {
				ep.Rules = append(ep.Rules, types.L7Rule{Allow: &types.L7Allow{OperationType: r.OperationType, Fields: r.Fields}})
			}
		case len(e.Rules) > 0:
			ep.Protocol, ep.Enforcement = "rest", types.NetworkEnforcementModeEnforce
			for _, r := range e.Rules {
				ep.Rules = append(ep.Rules, types.L7Rule{Allow: &types.L7Allow{Method: r.Method, Path: r.Path}})
			}
		}
		provider := g.Provider
		if provider == "" {
			provider = bindings[sandbox.Endpoint{Host: e.Host, Port: e.Port}]
		}
		if provider != "" {
			ep.CredentialBinding = &types.NetworkCredentialBinding{Provider: provider}
		}
		rule.Endpoints = append(rule.Endpoints, ep)
	}
	return rule
}

// checkGrantProviders refuses a grant whose provider the session's git access doesn't sync, which could never authenticate.
func checkGrantProviders(grants []sandbox.Grant, access []sandbox.GitAccess) error {
	for _, g := range grants {
		if g.Provider != "" && !slices.ContainsFunc(access, func(a sandbox.GitAccess) bool { return a.Provider == g.Provider }) {
			return fmt.Errorf("grant %q needs provider %q, which the session's git access doesn't have", g.Name, g.Provider)
		}
	}
	return nil
}

// gitPolicyRule merges accesses to the same provider and endpoint into one endpoint, because the gateway matches an
// endpoint once. The binding is what gives the sandbox the provider's credential placeholder.
func gitPolicyRule(access []sandbox.GitAccess) types.NetworkPolicyRule {
	type key struct {
		provider, host string
		port           uint32
	}
	var (
		order     []key
		endpoints = map[key]*types.PolicyNetworkEndpoint{}
		seen      = map[key]map[sandbox.HTTPRule]bool{}
		binaries  []types.PolicyNetworkBinary
	)
	for _, g := range access {
		k := key{g.Provider, g.Endpoint.Host, g.Endpoint.Port}
		ep := endpoints[k]
		if ep == nil {
			ep = newGitEndpoint(g)
			endpoints[k] = ep
			seen[k] = map[sandbox.HTTPRule]bool{}
			order = append(order, k)
		}
		appendUniqueHTTPRules(ep, seen[k], g.Rules)
		binaries = appendUniqueBinaries(binaries, g.Binaries)
	}
	rule := types.NetworkPolicyRule{Name: gitRule, Binaries: binaries}
	for _, k := range order {
		rule.Endpoints = append(rule.Endpoints, *endpoints[k])
	}
	return rule
}

func newGitEndpoint(g sandbox.GitAccess) *types.PolicyNetworkEndpoint {
	ep := &types.PolicyNetworkEndpoint{
		Host: g.Endpoint.Host, Port: g.Endpoint.Port, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce,
	}
	// An anonymous access has no provider, so it reaches public repositories only.
	if g.Provider != "" {
		ep.CredentialBinding = &types.NetworkCredentialBinding{Provider: g.Provider}
	}
	return ep
}

func appendUniqueHTTPRules(ep *types.PolicyNetworkEndpoint, seen map[sandbox.HTTPRule]bool, rules []sandbox.HTTPRule) {
	for _, r := range rules {
		if !seen[r] {
			seen[r] = true
			ep.Rules = append(ep.Rules, types.L7Rule{Allow: &types.L7Allow{Method: r.Method, Path: r.Path}})
		}
	}
}

func appendUniqueBinaries(dst []types.PolicyNetworkBinary, paths []string) []types.PolicyNetworkBinary {
	for _, b := range paths {
		bin := types.PolicyNetworkBinary{Path: b}
		if !slices.Contains(dst, bin) {
			dst = append(dst, bin)
		}
	}
	return dst
}

// gitProviders returns the providers the sandbox needs, sorted and unique.
func gitProviders(access []sandbox.GitAccess) []string {
	var names []string
	for _, g := range access {
		if g.Provider != "" {
			names = append(names, g.Provider)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// gitEnv numbers the git client's config entries in key order so the environment is the same on every create.
func gitEnv(base map[string]string, access []sandbox.GitAccess) map[string]string {
	conf := maps.Clone(base)
	if conf == nil {
		conf = map[string]string{}
	}
	for _, g := range access {
		maps.Copy(conf, g.GitConfig)
	}
	if len(conf) == 0 {
		return nil
	}
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(conf))}
	for i, k := range slices.Sorted(maps.Keys(conf)) {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = k
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = conf[k]
	}
	return env
}
