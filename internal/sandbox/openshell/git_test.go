package openshell

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
)

// recorder wraps the fake, whose profile client is unimplemented, with an in-memory one that counts calls.
type recorder struct {
	*fake.Client
	profiles map[string]types.ProviderProfile
	imports  int
	updates  int
	ensures  int
}

func newRecorder() *recorder {
	return &recorder{Client: fake.NewClient(), profiles: map[string]types.ProviderProfile{}}
}

func (r *recorder) Providers() v1.ProviderInterface {
	return recordingProviders{ProviderInterface: r.Client.Providers(), r: r}
}

type recordingProviders struct {
	v1.ProviderInterface
	r *recorder
}

func (p recordingProviders) Profiles() v1.ProfileInterface { return recordingProfiles{r: p.r} }

func (p recordingProviders) Ensure(ctx context.Context, ws string, pr *types.Provider) (*types.Provider, error) {
	p.r.ensures++
	return p.ProviderInterface.Ensure(ctx, ws, pr)
}

type recordingProfiles struct {
	v1.ProfileInterface
	r *recorder
}

func (p recordingProfiles) Get(_ context.Context, ws, id string) (*types.ProviderProfile, error) {
	pr, ok := p.r.profiles[ws+"/"+id]
	if !ok {
		return nil, &types.StatusError{Code: types.ErrorNotFound, Message: "no such profile"}
	}
	return &pr, nil
}

func (p recordingProfiles) Update(_ context.Context, ws, id string, _ uint64, item types.ProfileImportItem) (*types.UpdateResult, error) {
	p.r.updates++
	p.r.profiles[ws+"/"+id] = item.Profile
	return &types.UpdateResult{Updated: true}, nil
}

func (p recordingProfiles) Import(_ context.Context, ws string, items []types.ProfileImportItem) (*types.ImportResult, error) {
	p.r.imports++
	for _, it := range items {
		p.r.profiles[ws+"/"+it.Profile.ID] = it.Profile
	}
	return &types.ImportResult{Imported: true}, nil
}

func tokenFn(token *string) map[string]func(context.Context) (string, error) {
	return map[string]func(context.Context) (string, error){"github": func(context.Context) (string, error) { return *token, nil }}
}

func repoAccess(t *testing.T, raw string) sandbox.GitAccess {
	t.Helper()
	h := github.New("")
	repo, err := h.ParseRepo(raw)
	require.NoError(t, err)
	return h.Access(repo)
}

func TestCreateWithGitRecordsProviderPolicyAndEnvironment(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	access := repoAccess(t, "github.com/octo/hello")

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}}))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, []string{"durable-github"}, sb.Spec.Providers)
	require.Equal(t, map[string]string{
		worker.TokenEnv:      "wt",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0": `!f() { echo username=x-access-token; echo "password=$GITHUB_TOKEN"; }; f`,
	}, sb.Spec.Environment)
	require.NotContains(t, sb.Spec.Environment, "GITHUB_TOKEN", "the sandbox gets a placeholder from the provider, not the token")

	p := sb.Spec.Policy
	require.NotNil(t, p)
	require.Equal(t, &types.LandlockPolicy{Compatibility: "best_effort"}, p.Landlock)
	var rules []types.L7Rule
	for _, r := range access.Rules {
		rules = append(rules, types.L7Rule{Allow: &types.L7Allow{Method: r.Method, Path: r.Path}})
	}
	require.Equal(t, map[string]types.NetworkPolicyRule{"durable-git": {
		Name: "durable-git",
		Endpoints: []types.PolicyNetworkEndpoint{{
			Host: "github.com", Port: 443, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce, Rules: rules,
			CredentialBinding: &types.NetworkCredentialBinding{Provider: "durable-github"},
		}},
		Binaries: []types.PolicyNetworkBinary{{Path: "/usr/bin/git"}, {Path: "/usr/lib/git-core/git-remote-https"}},
	}}, p.NetworkPolicies)

	prov, err := rec.Client.Providers().Get(t.Context(), "default", "durable-github")
	require.NoError(t, err)
	require.Equal(t, "durable-github", prov.Type)
	require.Equal(t, "default", prov.Spec.ProfileWorkspace)
	require.Equal(t, map[string]string{"GITHUB_TOKEN": "ghp_one"}, prov.Spec.Credentials)
	prof := rec.profiles["default/durable-github"]
	require.Equal(t, types.ProfileCategorySourceControl, prof.Category)
	require.Empty(t, prof.Endpoints)
	require.Len(t, prof.Credentials, 1)
	require.Equal(t, "GITHUB_TOKEN", prof.Credentials[0].Name)
}

func TestCreateWithGitAndEgressKeepsBothRules(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	spec := sandbox.Spec{
		Name: "dt-a", Image: "img", Token: "wt",
		Egress: []sandbox.Endpoint{{Host: "mcp.example.com", Port: 443}},
		Git:    []sandbox.GitAccess{repoAccess(t, "github.com/octo/one"), repoAccess(t, "github.com/octo/two")},
	}

	require.NoError(t, l.Create(t.Context(), spec))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	rules := sb.Spec.Policy.NetworkPolicies
	require.Len(t, rules, 2)
	require.Equal(t, []types.PolicyNetworkEndpoint{{Host: "mcp.example.com", Port: 443}}, rules["durable-mcp"].Endpoints)
	git := rules["durable-git"]
	require.Len(t, git.Endpoints, 1, "the same provider and endpoint merge into one")
	require.Len(t, git.Endpoints[0].Rules, 12)
	require.Len(t, git.Binaries, 2)
	require.Equal(t, []string{"durable-github"}, sb.Spec.Providers)
	require.Equal(t, "1", sb.Spec.Environment["GIT_CONFIG_COUNT"])
}

func TestGitEnvNumbersKeysInSortedOrder(t *testing.T) {
	env := gitEnv([]sandbox.GitAccess{
		{GitConfig: map[string]string{"b.key": "vb", "a.key": "va"}},
		{GitConfig: map[string]string{"c.key": "vc"}},
	})
	require.Equal(t, map[string]string{
		"GIT_CONFIG_COUNT": "3",
		"GIT_CONFIG_KEY_0": "a.key", "GIT_CONFIG_VALUE_0": "va",
		"GIT_CONFIG_KEY_1": "b.key", "GIT_CONFIG_VALUE_1": "vb",
		"GIT_CONFIG_KEY_2": "c.key", "GIT_CONFIG_VALUE_2": "vc",
	}, env)
}

func TestCreateWithoutGitTouchesNoProviders(t *testing.T) {
	rec := newRecorder()
	l := newWithClient(rec, "", nil)

	require.NoError(t, l.Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt"}))

	sb, err := rec.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Empty(t, sb.Spec.Providers)
	require.Equal(t, map[string]string{worker.TokenEnv: "wt"}, sb.Spec.Environment)
	require.Nil(t, sb.Spec.Policy)
	require.Zero(t, rec.ensures)
	require.Zero(t, rec.imports)
}

func TestEnsureSyncsProviderOnlyWhenTheTokenChanges(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}
	version := func() uint64 {
		p, err := rec.Client.Providers().Get(t.Context(), "default", "durable-github")
		require.NoError(t, err)
		return p.ResourceVersion
	}

	require.NoError(t, l.Ensure(t.Context(), spec), "creates")
	require.Equal(t, 1, rec.ensures)
	require.Equal(t, 1, rec.imports)
	v := version()

	require.NoError(t, l.Ensure(t.Context(), spec), "starts the existing sandbox")
	require.Equal(t, 1, rec.ensures, "the same token is not sent again")
	require.Equal(t, v, version())

	token = "ghp_two"
	require.NoError(t, l.Ensure(t.Context(), spec))
	require.Equal(t, 2, rec.ensures)
	require.Equal(t, 1, rec.imports, "the profile is imported once")
	require.Greater(t, version(), v)
	prov, err := rec.Client.Providers().Get(t.Context(), "default", "durable-github")
	require.NoError(t, err)
	require.Equal(t, "ghp_two", prov.Spec.Credentials["GITHUB_TOKEN"])

	// A new launcher has no hashes, so its first call always ensures.
	require.NoError(t, newWithClient(rec, "", tokenFn(&token)).Ensure(t.Context(), spec))
	require.Equal(t, 3, rec.ensures)
	require.Equal(t, 1, rec.imports, "an existing profile is not imported again")
}

func TestEnsureExistingSandboxSyncsProvider(t *testing.T) {
	rec := newRecorder()
	seed(rec.Client, types.SandboxStopped)
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))

	require.NoError(t, l.Ensure(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}))

	prov, err := rec.Client.Providers().Get(t.Context(), "default", "durable-github")
	require.NoError(t, err)
	require.Equal(t, "ghp_one", prov.Spec.Credentials["GITHUB_TOKEN"])
	phase, err := l.Phase(t.Context(), "dt-a")
	require.NoError(t, err)
	require.Equal(t, types.SandboxReady, phase)
}

func TestTokenFailureCreatesNothing(t *testing.T) {
	rec := newRecorder()
	boom := errors.New("op is locked")
	tokens := map[string]func(context.Context) (string, error){"github": func(context.Context) (string, error) { return "", boom }}
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}

	err := newWithClient(rec, "", tokens).Ensure(t.Context(), spec)

	require.ErrorIs(t, err, boom)
	_, perr := newWithClient(rec, "", nil).Phase(t.Context(), "dt-a")
	require.ErrorIs(t, perr, ErrNotFound)
	require.Zero(t, rec.ensures)
}

func TestMissingTokenFuncIsConfiguration(t *testing.T) {
	rec := newRecorder()
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}

	err := newWithClient(rec, "", nil).Ensure(t.Context(), spec)

	require.ErrorContains(t, err, "no github token")
	require.False(t, IsUnavailable(err))
	_, perr := newWithClient(rec, "", nil).Phase(t.Context(), "dt-a")
	require.ErrorIs(t, perr, ErrNotFound)
}

func TestFailedCreateResyncsTheProviderNextTime(t *testing.T) {
	rec := newRecorder()
	token := "ghp_one"
	l := newWithClient(rec, "", tokenFn(&token))
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}
	require.NoError(t, l.Create(t.Context(), spec))
	require.Equal(t, 1, rec.ensures)

	// The name is taken, so this create fails after the cached sync skipped the provider.
	require.Error(t, l.Create(t.Context(), spec))
	require.Equal(t, 1, rec.ensures)

	spec.Name = "dt-b"
	require.NoError(t, l.Create(t.Context(), spec))
	require.Equal(t, 2, rec.ensures, "a provider deleted on the gateway is restored after a failed create")
}

func TestStaleProfileIsUpdatedAndAMatchingOneIsKept(t *testing.T) {
	rec := newRecorder()
	rec.profiles["default/durable-github"] = types.ProviderProfile{ID: "durable-github", Credentials: []types.ProfileCredential{{Name: "GITHUB_TOKEN", AuthStyle: "basic"}}}
	token := "ghp_one"
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{repoAccess(t, "github.com/octo/hello")}}
	require.NoError(t, newWithClient(rec, "", tokenFn(&token)).Create(t.Context(), spec))
	require.Equal(t, 1, rec.updates, "a profile with another credential definition is replaced")
	require.Zero(t, rec.imports)
	require.Equal(t, "bearer", rec.profiles["default/durable-github"].Credentials[0].AuthStyle)

	spec.Name = "dt-b"
	require.NoError(t, newWithClient(rec, "", tokenFn(&token)).Create(t.Context(), spec))
	require.Equal(t, 1, rec.updates, "a matching profile is left alone")
}
