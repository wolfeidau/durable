package openshell

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"
	dm "github.com/ai-openshell/sdk-go/proto/datamodelv1"
	pb "github.com/ai-openshell/sdk-go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	refreshHashAnnotation = "durable.dev/refresh-hash"
	appRefreshBefore      = 5 * time.Minute
	appMaxLifetime        = time.Hour
)

// AppCredentials holds the GitHub App credentials passed to OpenShell's gateway-managed refresh.
type AppCredentials struct {
	ClientID   string
	PrivateKey string
}

// appRefreshClient wraps the raw proto/openshellv1 RPCs needed for github_app_installation until sdk-go's curated v1 layer maps the strategy enum.
type appRefreshClient interface {
	GetProfile(ctx context.Context, workspace, id string) (*pb.ProviderProfile, error)
	ImportProfile(ctx context.Context, workspace string, item *pb.ProviderProfileImportItem) error
	UpdateProfile(ctx context.Context, workspace, id string, expectedVersion uint64, item *pb.ProviderProfileImportItem) error
	GetRefreshStatus(ctx context.Context, workspace, provider, credentialKey string) ([]*pb.ProviderCredentialRefreshStatus, error)
	ConfigureRefresh(ctx context.Context, workspace string, req *pb.ConfigureProviderRefreshRequest) (*pb.ProviderCredentialRefreshStatus, error)
	RotateCredential(ctx context.Context, workspace, provider, credentialKey string) (*pb.ProviderCredentialRefreshStatus, error)
	Close() error
}

type grpcAppRefreshClient struct {
	conn   *grpc.ClientConn
	client pb.OpenShellClient
}

func dialAppRefreshClient(address string, tlsCfg *types.TLSConfig) (*grpcAppRefreshClient, error) {
	var dialOpt grpc.DialOption
	if after, ok := strings.CutPrefix(address, "http://"); ok {
		address = after
		dialOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		address = strings.TrimPrefix(address, "https://")
		creds, err := buildMTLS(tlsCfg)
		if err != nil {
			return nil, err
		}
		dialOpt = grpc.WithTransportCredentials(creds)
	}
	conn, err := grpc.NewClient(address, dialOpt)
	if err != nil {
		return nil, fmt.Errorf("failed to dial gateway for app refresh: %w", err)
	}
	return &grpcAppRefreshClient{conn: conn, client: pb.NewOpenShellClient(conn)}, nil
}

func buildMTLS(cfg *types.TLSConfig) (credentials.TransportCredentials, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg == nil {
		return credentials.NewTLS(tlsConfig), nil
	}
	tlsConfig.InsecureSkipVerify = cfg.Insecure
	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read gateway CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, errors.New("invalid gateway CA certificate")
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load gateway client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(tlsConfig), nil
}

func workspaceSelector(ws string) *dm.WorkspaceSelector {
	return &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: ws}}
}

func (c *grpcAppRefreshClient) GetProfile(ctx context.Context, ws, id string) (*pb.ProviderProfile, error) {
	resp, err := c.client.GetProviderProfile(ctx, &pb.GetProviderProfileRequest{
		Id:             id,
		WorkspaceScope: workspaceSelector(ws),
	})
	if err != nil {
		return nil, mapGRPCError(err)
	}
	return resp.GetProfile(), nil
}

func (c *grpcAppRefreshClient) ImportProfile(ctx context.Context, ws string, item *pb.ProviderProfileImportItem) error {
	resp, err := c.client.ImportProviderProfiles(ctx, &pb.ImportProviderProfilesRequest{
		Profiles:       []*pb.ProviderProfileImportItem{item},
		WorkspaceScope: workspaceSelector(ws),
	})
	if err != nil {
		return mapGRPCError(err)
	}
	if !resp.GetImported() {
		return profileDiagnosticsError("import", resp.GetDiagnostics())
	}
	return nil
}

func (c *grpcAppRefreshClient) UpdateProfile(ctx context.Context, ws, id string, expectedVersion uint64, item *pb.ProviderProfileImportItem) error {
	resp, err := c.client.UpdateProviderProfiles(ctx, &pb.UpdateProviderProfilesRequest{
		Id:                      id,
		Profile:                 item,
		ExpectedResourceVersion: expectedVersion,
		WorkspaceScope:          workspaceSelector(ws),
	})
	if err != nil {
		return mapGRPCError(err)
	}
	if !resp.GetUpdated() {
		return profileDiagnosticsError("update", resp.GetDiagnostics())
	}
	return nil
}

func (c *grpcAppRefreshClient) GetRefreshStatus(ctx context.Context, ws, provider, credentialKey string) ([]*pb.ProviderCredentialRefreshStatus, error) {
	resp, err := c.client.GetProviderRefreshStatus(ctx, &pb.GetProviderRefreshStatusRequest{
		Provider:       provider,
		CredentialKey:  credentialKey,
		WorkspaceScope: workspaceSelector(ws),
	})
	if err != nil {
		return nil, mapGRPCError(err)
	}
	return resp.GetCredentials(), nil
}

func (c *grpcAppRefreshClient) ConfigureRefresh(ctx context.Context, ws string, req *pb.ConfigureProviderRefreshRequest) (*pb.ProviderCredentialRefreshStatus, error) {
	req.WorkspaceScope = workspaceSelector(ws)
	resp, err := c.client.ConfigureProviderRefresh(ctx, req)
	if err != nil {
		return nil, mapGRPCError(err)
	}
	return resp.GetStatus(), nil
}

func (c *grpcAppRefreshClient) RotateCredential(ctx context.Context, ws, provider, credentialKey string) (*pb.ProviderCredentialRefreshStatus, error) {
	resp, err := c.client.RotateProviderCredential(ctx, &pb.RotateProviderCredentialRequest{
		Provider:       provider,
		CredentialKey:  credentialKey,
		WorkspaceScope: workspaceSelector(ws),
	})
	if err != nil {
		return nil, mapGRPCError(err)
	}
	return resp.GetStatus(), nil
}

func (c *grpcAppRefreshClient) Close() error { return c.conn.Close() }

func profileDiagnosticsError(op string, diags []*pb.ProviderProfileDiagnostic) error {
	var msgs []string
	for _, d := range diags {
		msgs = append(msgs, fmt.Sprintf("%s: %s", d.GetField(), d.GetMessage()))
	}
	if len(msgs) == 0 {
		return fmt.Errorf("gateway rejected profile %s", op)
	}
	return fmt.Errorf("gateway rejected profile %s: %s", op, strings.Join(msgs, "; "))
}

// mapGRPCError converts gRPC status errors into sdk-go StatusError so v1.IsNotFound and IsUnavailable work consistently.
func mapGRPCError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	var code types.ErrorCode
	switch st.Code() {
	case codes.NotFound:
		code = types.ErrorNotFound
	case codes.InvalidArgument:
		code = types.ErrorInvalidArgument
	case codes.AlreadyExists:
		code = types.ErrorAlreadyExists
	case codes.PermissionDenied:
		code = types.ErrorPermissionDenied
	case codes.Unauthenticated:
		code = types.ErrorUnauthenticated
	case codes.Unavailable:
		code = types.ErrorUnavailable
	case codes.DeadlineExceeded:
		code = types.ErrorDeadlineExceeded
	case codes.FailedPrecondition, codes.Aborted:
		code = types.ErrorConflict
	default:
		code = types.ErrorInternal
	}
	return &types.StatusError{Code: code, Message: st.Message()}
}

func appProfileItem(g sandbox.GitAccess, tokenURL string) *pb.ProviderProfileImportItem {
	if tokenURL == "" {
		tokenURL = github.DefaultAppTokenURL
	}
	return &pb.ProviderProfileImportItem{
		Source: "durable",
		Profile: &pb.ProviderProfile{
			Id:          g.ProfileID,
			DisplayName: "durable " + g.Kind + " app",
			Category:    pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL,
			Credentials: []*pb.ProviderProfileCredential{{
				Name:       g.CredentialEnv,
				EnvVars:    []string{g.CredentialEnv},
				Required:   true,
				AuthStyle:  "bearer",
				HeaderName: "authorization",
				Refresh: &pb.ProviderCredentialRefresh{
					Strategy:      pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION,
					TokenUrl:      tokenURL,
					RefreshBefore: durationpb.New(appRefreshBefore),
					MaxLifetime:   durationpb.New(appMaxLifetime),
					Material: []*pb.ProviderCredentialRefreshMaterial{
						{Name: "client_id", Description: "GitHub App client ID used as JWT issuer", Required: true},
						{Name: "installation_id", Description: "Positive GitHub App installation ID", Required: true},
						{Name: "private_key", Description: "GitHub App RSA PEM private key", Required: true, Secret: true},
						{Name: "repository_ids", Description: "JSON array of positive repository IDs", Required: true},
						{Name: "permissions", Description: "JSON object of GitHub permission names and levels", Required: true},
					},
				},
			}},
		},
	}
}

func sameAppProfile(got, want *pb.ProviderProfile) bool {
	if got == nil || want == nil {
		return false
	}
	return slices.EqualFunc(got.GetCredentials(), want.GetCredentials(), sameAppCredential)
}

func sameAppCredential(a, b *pb.ProviderProfileCredential) bool {
	return a.GetName() == b.GetName() &&
		slices.Equal(a.GetEnvVars(), b.GetEnvVars()) &&
		a.GetRequired() == b.GetRequired() &&
		a.GetAuthStyle() == b.GetAuthStyle() &&
		a.GetHeaderName() == b.GetHeaderName() &&
		sameAppRefresh(a.GetRefresh(), b.GetRefresh())
}

func sameAppRefresh(a, b *pb.ProviderCredentialRefresh) bool {
	if a == nil || b == nil {
		return false
	}
	return a.GetStrategy() == b.GetStrategy() &&
		a.GetTokenUrl() == b.GetTokenUrl() &&
		a.GetRefreshBefore().AsDuration() == b.GetRefreshBefore().AsDuration() &&
		a.GetMaxLifetime().AsDuration() == b.GetMaxLifetime().AsDuration() &&
		slices.EqualFunc(a.GetMaterial(), b.GetMaterial(), sameRefreshMaterial)
}

func sameRefreshMaterial(a, b *pb.ProviderCredentialRefreshMaterial) bool {
	return a.GetName() == b.GetName() &&
		a.GetRequired() == b.GetRequired() &&
		a.GetSecret() == b.GetSecret()
}

func isRefreshHealthy(statuses []*pb.ProviderCredentialRefreshStatus, credentialKey string) bool {
	for _, s := range statuses {
		if s.GetCredentialKey() != credentialKey {
			continue
		}
		if s.GetStrategy() != pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION {
			continue
		}
		if s.GetLastError() != "" || s.GetFailureCode() != "" {
			return false
		}
		if s.GetExpirationTime() != nil && s.GetExpirationTime().AsTime().After(time.Now()) {
			return true
		}
	}
	return false
}

// syncAppProvider configures gateway-managed GitHub App token refresh for g and mints immediately on first setup or change.
func (l *Launcher) syncAppProvider(ctx context.Context, g sandbox.GitAccess) error {
	appFn := l.apps[g.Kind]
	if appFn == nil {
		return fmt.Errorf("no %s app is configured for this server", g.Kind)
	}
	if l.appRPC == nil {
		return errors.New("gateway client does not support GitHub App refresh")
	}
	if g.InstallationID <= 0 || g.RepositoryID <= 0 || len(g.Permissions) == 0 {
		return fmt.Errorf("incomplete %s app scope for provider %q", g.Kind, g.Provider)
	}
	if err := l.ensureAppProfile(ctx, g); err != nil {
		return err
	}
	appCreds, err := appFn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get %s app credentials: %w", g.Kind, err)
	}
	repoIDsJSON := fmt.Sprintf("[%d]", g.RepositoryID)
	permsJSON, err := json.Marshal(g.Permissions)
	if err != nil {
		return fmt.Errorf("failed to encode %s app permissions: %w", g.Kind, err)
	}
	sum := appMaterialHash(appCreds, g.InstallationID, repoIDsJSON, permsJSON, l.appTokenURL)
	if last, ok := l.synced[g.Provider]; ok && last == sum {
		return nil
	}
	hexSum := hex.EncodeToString(sum[:])

	// Reconfiguring withdraws active handles immediately, so after a server restart skip it when the gateway already holds the same healthy config.
	if l.isAppProviderHealthy(ctx, g, hexSum) {
		l.synced[g.Provider] = sum
		return nil
	}

	_, err = l.c.Providers().Ensure(ctx, l.ws, &types.Provider{
		Name:        g.Provider,
		Type:        g.ProfileID,
		Annotations: map[string]string{refreshHashAnnotation: hexSum},
		Spec:        types.ProviderSpec{ProfileWorkspace: l.ws},
	})
	if err != nil {
		return fmt.Errorf("failed to set %s app provider: %w", g.Kind, err)
	}
	_, err = l.appRPC.ConfigureRefresh(ctx, l.ws, &pb.ConfigureProviderRefreshRequest{
		Provider:      g.Provider,
		CredentialKey: g.CredentialEnv,
		Strategy:      pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_GITHUB_APP_INSTALLATION,
		Material: map[string]string{
			"client_id":       appCreds.ClientID,
			"installation_id": strconv.FormatInt(g.InstallationID, 10),
			"private_key":     appCreds.PrivateKey,
			"repository_ids":  repoIDsJSON,
			"permissions":     string(permsJSON),
		},
		SecretMaterialKeys: []string{"private_key"},
	})
	if err != nil {
		return fmt.Errorf("failed to configure %s app refresh (requires OpenShell gateway >= v0.1.10): %w", g.Kind, err)
	}
	st, err := l.appRPC.RotateCredential(ctx, l.ws, g.Provider, g.CredentialEnv)
	if err != nil {
		return fmt.Errorf("failed to mint %s app installation token: %w", g.Kind, err)
	}
	if st.GetLastError() != "" || st.GetFailureCode() != "" {
		return fmt.Errorf("failed to mint %s app installation token: %s (%s)", g.Kind, st.GetLastError(), st.GetFailureCode())
	}
	l.synced[g.Provider] = sum
	return nil
}

func appMaterialHash(creds AppCredentials, installationID int64, repoIDsJSON string, permsJSON []byte, tokenURL string) [sha256.Size]byte {
	return sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s\x00%s\x00%s",
		creds.ClientID, installationID, creds.PrivateKey, repoIDsJSON, permsJSON, tokenURL))
}

func (l *Launcher) isAppProviderHealthy(ctx context.Context, g sandbox.GitAccess, hexSum string) bool {
	existing, err := l.c.Providers().Get(ctx, l.ws, g.Provider)
	if err != nil || existing.Type != g.ProfileID || existing.Annotations[refreshHashAnnotation] != hexSum {
		return false
	}
	statuses, err := l.appRPC.GetRefreshStatus(ctx, l.ws, g.Provider, g.CredentialEnv)
	return err == nil && isRefreshHealthy(statuses, g.CredentialEnv)
}

func (l *Launcher) ensureAppProfile(ctx context.Context, g sandbox.GitAccess) error {
	if l.profiled[g.ProfileID] {
		return nil
	}
	item := appProfileItem(g, l.appTokenURL)
	existing, err := l.appRPC.GetProfile(ctx, l.ws, g.ProfileID)
	switch {
	case err == nil && sameAppProfile(existing, item.GetProfile()):
	case err == nil:
		if err := l.appRPC.UpdateProfile(ctx, l.ws, g.ProfileID, existing.GetResourceVersion(), item); err != nil {
			return fmt.Errorf("failed to update %s app profile: %w", g.Kind, err)
		}
	case v1.IsNotFound(err):
		if err := l.appRPC.ImportProfile(ctx, l.ws, item); err != nil {
			return fmt.Errorf("failed to import %s app profile (requires OpenShell gateway >= v0.1.10): %w", g.Kind, err)
		}
	default:
		return fmt.Errorf("failed to get %s app profile: %w", g.Kind, err)
	}
	l.profiled[g.ProfileID] = true
	return nil
}
