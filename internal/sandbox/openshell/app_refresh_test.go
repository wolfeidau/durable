package openshell

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	pb "github.com/ai-openshell/sdk-go/proto/openshellv1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

type fakeAppRefresh struct {
	profiles   map[string]*pb.ProviderProfile
	configs    map[string]*pb.ConfigureProviderRefreshRequest
	statuses   map[string][]*pb.ProviderCredentialRefreshStatus
	imports    int
	updates    int
	configures int
	rotates    int
	rotateErr  error
	rotateResp *pb.ProviderCredentialRefreshStatus
}

func newFakeAppRefresh() *fakeAppRefresh {
	return &fakeAppRefresh{
		profiles: map[string]*pb.ProviderProfile{},
		configs:  map[string]*pb.ConfigureProviderRefreshRequest{},
		statuses: map[string][]*pb.ProviderCredentialRefreshStatus{},
	}
}

func (f *fakeAppRefresh) GetProfile(_ context.Context, ws, id string) (*pb.ProviderProfile, error) {
	p, ok := f.profiles[ws+"/"+id]
	if !ok {
		return nil, &types.StatusError{Code: types.ErrorNotFound, Message: "no such profile"}
	}
	return p, nil
}

func (f *fakeAppRefresh) ImportProfile(_ context.Context, ws string, item *pb.ProviderProfileImportItem) error {
	f.imports++
	p := item.GetProfile()
	p.ResourceVersion = 1
	f.profiles[ws+"/"+p.GetId()] = p
	return nil
}

func (f *fakeAppRefresh) UpdateProfile(_ context.Context, ws, id string, expected uint64, item *pb.ProviderProfileImportItem) error {
	f.updates++
	p := item.GetProfile()
	p.ResourceVersion = expected + 1
	f.profiles[ws+"/"+id] = p
	return nil
}

func (f *fakeAppRefresh) GetRefreshStatus(_ context.Context, ws, provider, key string) ([]*pb.ProviderCredentialRefreshStatus, error) {
	return f.statuses[ws+"/"+provider+"/"+key], nil
}

func (f *fakeAppRefresh) ConfigureRefresh(_ context.Context, ws string, req *pb.ConfigureProviderRefreshRequest) (*pb.ProviderCredentialRefreshStatus, error) {
	f.configures++
	key := ws + "/" + req.GetProvider() + "/" + req.GetCredentialKey()
	f.configs[key] = req
	st := &pb.ProviderCredentialRefreshStatus{
		Provider:      req.GetProvider(),
		CredentialKey: req.GetCredentialKey(),
		Strategy:      req.GetStrategy(),
		Status:        "pending",
	}
	f.statuses[key] = []*pb.ProviderCredentialRefreshStatus{st}
	return st, nil
}

func (f *fakeAppRefresh) RotateCredential(_ context.Context, ws, provider, credKey string) (*pb.ProviderCredentialRefreshStatus, error) {
	f.rotates++
	if f.rotateErr != nil {
		return nil, f.rotateErr
	}
	key := ws + "/" + provider + "/" + credKey
	st := f.rotateResp
	if st == nil {
		st = &pb.ProviderCredentialRefreshStatus{
			Provider:       provider,
			CredentialKey:  credKey,
			Strategy:       pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION,
			Status:         "active",
			ExpirationTime: timestamppb.New(time.Now().Add(time.Hour)),
		}
	}
	f.statuses[key] = []*pb.ProviderCredentialRefreshStatus{st}
	return st, nil
}

func (*fakeAppRefresh) Close() error { return nil }

func appRepoAccess(t *testing.T, raw string, instID, repoID int64, perms map[string]string) sandbox.GitAccess {
	t.Helper()
	h := github.New("")
	repo, err := h.ParseRepo(raw)
	require.NoError(t, err)
	return h.AppAccess(repo, github.AppRepoScope{InstallationID: instID, RepositoryID: repoID, Permissions: perms})
}

func newAppLauncher(fc *fake.Client, rpc appRefreshClient, creds *AppCredentials) *Launcher {
	l := newWithClient(fc, "", nil)
	l.appRPC = rpc
	if creds != nil {
		l.apps = map[string]func(context.Context) (AppCredentials, error){
			"github": func(context.Context) (AppCredentials, error) { return *creds, nil },
		}
	}
	return l
}

func TestCreateWithGitHubAppConfiguresProfileProviderAndRefresh(t *testing.T) {
	fc := fake.NewClient()
	rpc := newFakeAppRefresh()
	creds := AppCredentials{ClientID: "Iv23liTEST", PrivateKey: "PEM-ONE"}
	l := newAppLauncher(fc, rpc, &creds)
	access := appRepoAccess(t, "github.com/octo/hello", 42, 123456, map[string]string{"contents": "write", "metadata": "read"})
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}}

	require.NoError(t, l.Create(t.Context(), spec))

	sb, err := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
	require.NoError(t, err)
	require.Equal(t, []string{access.Provider}, sb.Spec.Providers)
	require.Equal(t, &types.NetworkCredentialBinding{Provider: access.Provider}, sb.Spec.Policy.NetworkPolicies["durable-git"].Endpoints[0].CredentialBinding)

	prov, err := fc.Providers().Get(t.Context(), "default", access.Provider)
	require.NoError(t, err)
	require.Equal(t, github.AppProfile, prov.Type)
	require.Empty(t, prov.Spec.Credentials, "runtime-credentials provider must hold no static credentials")
	require.NotEmpty(t, prov.Annotations[refreshHashAnnotation])

	require.Equal(t, 1, rpc.imports)
	require.Equal(t, 1, rpc.configures)
	require.Equal(t, 1, rpc.rotates)
	cfg := rpc.configs["default/"+access.Provider+"/GITHUB_TOKEN"]
	require.Equal(t, pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION, cfg.GetStrategy())
	require.Equal(t, []string{"private_key"}, cfg.GetSecretMaterialKeys())
	require.Equal(t, "Iv23liTEST", cfg.GetMaterial()["client_id"])
	require.Equal(t, "42", cfg.GetMaterial()["installation_id"])
	require.Equal(t, "PEM-ONE", cfg.GetMaterial()["private_key"])
	require.Equal(t, "[123456]", cfg.GetMaterial()["repository_ids"])
	require.JSONEq(t, `{"contents":"write","metadata":"read"}`, cfg.GetMaterial()["permissions"])

	// Second Ensure on same launcher skips reconfiguration.
	require.NoError(t, l.Ensure(t.Context(), spec))
	require.Equal(t, 1, rpc.configures)
	require.Equal(t, 1, rpc.rotates)

	// Fresh launcher (server restart) with unchanged key and healthy gateway status skips reconfiguration so active tokens stay valid.
	l2 := newAppLauncher(fc, rpc, &creds)
	require.NoError(t, l2.Ensure(t.Context(), spec))
	require.Equal(t, 1, rpc.imports)
	require.Equal(t, 1, rpc.configures)
	require.Equal(t, 1, rpc.rotates)

	// Rotated key triggers reconfigure + immediate rotate.
	creds.PrivateKey = "PEM-TWO"
	require.NoError(t, l2.Ensure(t.Context(), spec))
	require.Equal(t, 2, rpc.configures)
	require.Equal(t, 2, rpc.rotates)
	require.Equal(t, "PEM-TWO", rpc.configs["default/"+access.Provider+"/GITHUB_TOKEN"].GetMaterial()["private_key"])

	// Unhealthy gateway status forces reconfigure even with unchanged key on a fresh launcher.
	rpc.statuses["default/"+access.Provider+"/GITHUB_TOKEN"] = []*pb.ProviderCredentialRefreshStatus{{
		Provider: access.Provider, CredentialKey: "GITHUB_TOKEN",
		Strategy:    pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION,
		LastError:   "expired",
		FailureCode: "github_auth_failed",
	}}
	require.NoError(t, newAppLauncher(fc, rpc, &creds).Ensure(t.Context(), spec))
	require.Equal(t, 3, rpc.configures)
	require.Equal(t, 3, rpc.rotates)
}

func TestGitHubAppStaleProfileIsUpdated(t *testing.T) {
	fc := fake.NewClient()
	rpc := newFakeAppRefresh()
	rpc.profiles["default/"+github.AppProfile] = &pb.ProviderProfile{
		Id: github.AppProfile, ResourceVersion: 3,
		Credentials: []*pb.ProviderProfileCredential{{Name: "GITHUB_TOKEN", AuthStyle: "basic"}},
	}
	creds := AppCredentials{ClientID: "Iv23liTEST", PrivateKey: "PEM"}
	access := appRepoAccess(t, "github.com/octo/hello", 42, 123456, map[string]string{"contents": "write", "metadata": "read"})

	require.NoError(t, newAppLauncher(fc, rpc, &creds).Create(t.Context(), sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}}))
	require.Zero(t, rpc.imports)
	require.Equal(t, 1, rpc.updates)
	require.Equal(t, uint64(4), rpc.profiles["default/"+github.AppProfile].GetResourceVersion())
}

func TestGitHubAppFailuresCreateNothing(t *testing.T) {
	access := appRepoAccess(t, "github.com/octo/hello", 42, 123456, map[string]string{"contents": "write", "metadata": "read"})
	spec := sandbox.Spec{Name: "dt-a", Image: "img", Token: "wt", Git: []sandbox.GitAccess{access}}

	t.Run("missing app config", func(t *testing.T) {
		fc := fake.NewClient()
		err := newAppLauncher(fc, newFakeAppRefresh(), nil).Create(t.Context(), spec)
		require.ErrorContains(t, err, "no github app is configured")
		require.False(t, IsUnavailable(err))
		_, perr := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
		require.Error(t, perr)
	})

	t.Run("rotate error", func(t *testing.T) {
		fc := fake.NewClient()
		rpc := newFakeAppRefresh()
		rpc.rotateErr = errors.New("github 401")
		creds := AppCredentials{ClientID: "Iv23liTEST", PrivateKey: "PEM"}
		err := newAppLauncher(fc, rpc, &creds).Create(t.Context(), spec)
		require.ErrorContains(t, err, "github 401")
		_, perr := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
		require.Error(t, perr)
	})

	t.Run("rotate failure status", func(t *testing.T) {
		fc := fake.NewClient()
		rpc := newFakeAppRefresh()
		rpc.rotateResp = &pb.ProviderCredentialRefreshStatus{
			LastError:   "bad permissions",
			FailureCode: "github_scope_rejected",
		}
		creds := AppCredentials{ClientID: "Iv23liTEST", PrivateKey: "PEM"}
		err := newAppLauncher(fc, rpc, &creds).Create(t.Context(), spec)
		require.ErrorContains(t, err, "github_scope_rejected")
		_, perr := fc.Sandboxes().Get(t.Context(), "default", "dt-a")
		require.Error(t, perr)
	})
}
