// Package server serves the durable Connect API and the embedded UI.
package server

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

//go:embed ui/dist
var uiFS embed.FS

// Config configures a Server.
type Config struct {
	Home   string
	Token  string
	Addr   string // the actual listen address, used for the Host allow list
	Logger *slog.Logger
	// Registry opens the sessions the handlers act on.
	Registry *Registry
}

// Server is the HTTP handler for the API and UI.
type Server struct {
	cfg Config
}

// New returns a Server.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// sessionService answers every RPC with Unimplemented until the slices behind it land.
type sessionService struct {
	durablev1connect.UnimplementedSessionServiceHandler
	reg    *Registry
	home   string
	logger *slog.Logger
}

// Handler returns the API and UI behind the token, Host and Origin checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	path, h := durablev1connect.NewSessionServiceHandler(sessionService{reg: s.cfg.Registry, home: s.cfg.Home, logger: s.logger()})
	mux.Handle(path, s.requireToken(h))

	ui, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		panic(err) // the embedded path is fixed at compile time
	}
	mux.Handle("/", http.FileServerFS(ui))

	return s.checkHostAndOrigin(mux)
}

func (s *Server) logger() *slog.Logger {
	if s.cfg.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.cfg.Logger
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	want := []byte(s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			// Connect, gRPC and gRPC-Web clients map 401 to Unauthenticated.
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedHosts() map[string]bool {
	hosts := map[string]bool{s.cfg.Addr: true}
	if _, port, err := net.SplitHostPort(s.cfg.Addr); err == nil {
		hosts["localhost:"+port] = true
		hosts["127.0.0.1:"+port] = true
	}
	return hosts
}

// checkHostAndOrigin blocks DNS rebinding and cross-site browser requests.
func (s *Server) checkHostAndOrigin(next http.Handler) http.Handler {
	hosts := s.allowedHosts()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || !hosts[u.Host] {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

var sessionID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// SessionDir maps a session ID to its directory. It's the only way an ID may reach the filesystem.
func SessionDir(home, id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid session id %q", id))
	}
	return filepath.Join(home, ".durable", "sessions", id), nil
}
