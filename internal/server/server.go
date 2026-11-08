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
	"time"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

// all: embeds ui/dist when it exists, which a fresh clone lacks.
//
//go:embed all:ui
var uiFS embed.FS

// uiRoot serves the built UI when it has an index.html, otherwise the committed placeholder.
func uiRoot(fsys fs.FS) fs.FS {
	dir := "ui/placeholder"
	if _, err := fs.Stat(fsys, "ui/dist/index.html"); err == nil {
		dir = "ui/dist"
	}
	ui, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // the path is valid by construction
	}
	return ui
}

// Config configures a Server.
type Config struct {
	Home   string
	Token  string
	Addr   string // the actual listen address, used for the Host allow list
	Logger *slog.Logger
	// Registry opens the sessions the handlers act on.
	Registry *Registry
	// Heartbeat is how often an idle watch stream sends a heartbeat. Zero means DefaultHeartbeat.
	Heartbeat time.Duration
	// Draining is closed when shutdown begins, so open streams end instead of stalling the drain. Nil never closes.
	Draining <-chan struct{}
}

// Server is the HTTP handler for the API and UI.
type Server struct {
	cfg Config
}

// New returns a Server.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// sessionService implements the RPCs whose slices have landed; the rest stay Unimplemented.
type sessionService struct {
	durablev1connect.UnimplementedSessionServiceHandler
	reg       *Registry
	home      string
	heartbeat time.Duration
	draining  <-chan struct{}
	logger    *slog.Logger
}

// Handler returns the API and UI behind the token, Host and Origin checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	heartbeat := s.cfg.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	path, h := durablev1connect.NewSessionServiceHandler(sessionService{
		reg: s.cfg.Registry, home: s.cfg.Home, heartbeat: heartbeat, draining: s.cfg.Draining, logger: s.logger(),
	})
	mux.Handle(path, s.requireToken(h))

	mux.Handle("/", http.FileServerFS(uiRoot(uiFS)))

	return s.checkHostAndOrigin(denyFraming(mux))
}

// denyFraming stops another site from framing the UI and tricking a click on Allow.
func denyFraming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
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
