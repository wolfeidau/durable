package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/wolfeidau/durable/internal/githost"
	"github.com/wolfeidau/durable/internal/sandbox"
)

// AppAccess returns sandbox GitAccess backed by an OpenShell GitHub App provider scoped to scope.
func (h *Host) AppAccess(repo githost.Repo, scope AppRepoScope) sandbox.GitAccess {
	a := h.Access(repo)
	a.AuthMode = sandbox.GitAuthModeGitHubApp
	a.ProfileID = AppProfile
	a.Provider = AppProviderName(scope.InstallationID, scope.RepositoryID, scope.Permissions)
	a.InstallationID = scope.InstallationID
	a.RepositoryID = scope.RepositoryID
	a.Permissions = maps.Clone(scope.Permissions)
	return a
}

// AppProviderName returns the deterministic OpenShell provider name for one (installation, repository, permissions) scope.
func AppProviderName(installationID, repoID int64, perms map[string]string) string {
	permsJSON, _ := json.Marshal(perms)
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%d:%s", installationID, repoID, permsJSON))
	return fmt.Sprintf("%s%d-%s", AppProviderPrefix, repoID, hex.EncodeToString(sum[:4]))
}

func capPermission(granted map[string]string, name, want string) (string, bool) {
	level := granted[name]
	switch want {
	case "write":
		if level == "write" || level == "admin" {
			return "write", true
		}
		if level == "read" {
			return "read", true
		}
	case "read":
		if level == "read" || level == "write" || level == "admin" {
			return "read", true
		}
	}
	return "", false
}

func scopedPermissions(granted map[string]string, withAPI bool) (map[string]string, error) {
	contents, ok := capPermission(granted, "contents", "write")
	if !ok {
		return nil, errors.New("installation is missing contents permission")
	}
	perms := map[string]string{
		"contents": contents,
		"metadata": "read",
	}
	if !withAPI {
		return perms, nil
	}
	// Cap each API permission at what the installation actually grants so both read-only and read-write apps work without GitHub 422s.
	for _, spec := range []struct{ name, want string }{
		{"pull_requests", "write"},
		{"issues", "write"},
		{"checks", "read"},
		{"actions", "read"},
	} {
		if level, ok := capPermission(granted, spec.name, spec.want); ok {
			perms[spec.name] = level
		}
	}
	return perms, nil
}
