package openshell

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
	if got == nil || want == nil || len(got.GetCredentials()) != len(want.GetCredentials()) {
		return false
	}
	for i := range want.GetCredentials() {
		a, b := got.GetCredentials()[i], want.GetCredentials()[i]
		if a.GetName() != b.GetName() || !slices.Equal(a.GetEnvVars(), b.GetEnvVars()) ||
			a.GetRequired() != b.GetRequired() || a.GetAuthStyle() != b.GetAuthStyle() ||
			a.GetHeaderName() != b.GetHeaderName() {
			return false
		}
		ar, br := a.GetRefresh(), b.GetRefresh()
		if ar == nil || br == nil {
			return false
		}
		if ar.GetStrategy() != br.GetStrategy() || ar.GetTokenUrl() != br.GetTokenUrl() ||
			ar.GetRefreshBefore().AsDuration() != br.GetRefreshBefore().AsDuration() ||
			ar.GetMaxLifetime().AsDuration() != br.GetMaxLifetime().AsDuration() {
			return false
		}
		if len(ar.GetMaterial()) != len(br.GetMaterial()) {
			return false
		}
		for j := range br.GetMaterial() {
			am, bm := ar.GetMaterial()[j], br.GetMaterial()[j]
			if am.GetName() != bm.GetName() || am.GetRequired() != bm.GetRequired() || am.GetSecret() != bm.GetSecret() {
				return false
			}
		}
	}
	return true
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
