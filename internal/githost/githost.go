// Package githost describes a git hosting service a session's repository can live on.
package githost

import (
	"context"

	"github.com/wolfeidau/durable/internal/sandbox"
)

// Repo is a repository in its canonical form.
type Repo struct {
	Host, Owner, Name string
}

func (r Repo) String() string { return r.Host + "/" + r.Owner + "/" + r.Name }

// TokenInfo describes a token as its host reports it.
type TokenInfo struct {
	Login string
	// Kind is the token's type, such as fine-grained or classic.
	Kind   string
	Scopes []string
}

// Host is a git hosting service.
type Host interface {
	Kind() string
	ParseRepo(raw string) (Repo, error)
	// Check asks the host who the token belongs to, so a bad token fails at startup.
	Check(ctx context.Context, token string) (TokenInfo, error)
	// Access returns what a sandbox needs to reach only repo.
	Access(repo Repo) sandbox.GitAccess
}
