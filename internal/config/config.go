// Package config reads the durable-server settings from ~/.durable/config.json or config.yaml and the command line flags.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/userfile"
)

const (
	// BaseName is the config file's name inside ~/.durable, before its .json, .yaml or .yml extension.
	BaseName = "config"

	legacyLabel = "~/.durable/environments.json"

	githubTokenEnv = "DURABLE_GITHUB_TOKEN" //nolint:gosec // a variable name, not a credential

	// DefaultGitAuthor is the commit identity of sandboxed sessions.
	DefaultGitAuthor = "durable <durable@localhost>"
)

// GitHostKinds lists the supported git hosts.
var GitHostKinds = []string{github.Kind}

// Secret reference patterns: a pasted value matches none of them.
const (
	secretRefPattern = `^(` + creds.OpRefPattern + `|gh|env:[A-Za-z_][A-Za-z0-9_]*|` + creds.FileRefPattern + `)$`
	apiKeyRefPattern = `^(` + creds.OpRefPattern + `|env:[A-Za-z_][A-Za-z0-9_]*|` + creds.FileRefPattern + `)$`
	durationPattern  = `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
)

var (
	secretRefRE = regexp.MustCompile(secretRefPattern)
	apiKeyRefRE = regexp.MustCompile(apiKeyRefPattern)
)

// OTel protocols; the first is the default.
const (
	OTelProtocolHTTP = "http/protobuf"
	OTelProtocolGRPC = "grpc"
)

// OTelProtocols lists the supported OTLP protocols.
var OTelProtocols = []string{OTelProtocolHTTP, OTelProtocolGRPC}

// Config is the shape of ~/.durable/config.json or config.yaml.
type Config struct {
	Schema       string                 `json:"$schema,omitempty" jsonschema:"URL of this schema, so editors can validate the file."`
	Listen       Listen                 `json:"listen,omitzero" jsonschema:"Where the server listens and how it times out clients."`
	Provider     Provider               `json:"provider,omitzero" jsonschema:"The model provider for new sessions. Existing sessions keep their pin."`
	Git          Git                    `json:"git,omitzero" jsonschema:"Git settings for sandboxed sessions."`
	OTel         OTel                   `json:"otel,omitzero" jsonschema:"Export traces over OTLP. Setting endpoint turns it on."`
	Environments map[string]Environment `json:"environments,omitempty" jsonschema:"Sandbox environments, by name. A session picks one; default is used when it names none."`
}

// OTel configures the trace exporter.
type OTel struct {
	Endpoint       string            `json:"endpoint,omitempty" jsonschema:"OTLP endpoint as an absolute http or https URL. Empty turns tracing off."`
	Protocol       string            `json:"protocol,omitempty" jsonschema:"OTLP protocol: http/protobuf (default) or grpc."`
	Headers        map[string]string `json:"headers,omitempty" jsonschema:"Headers sent with every export, by name. Each value is an op:// 1Password reference, env:NAME or file:PATH, never the value itself."`
	HeadersFrom    string            `json:"headers_from,omitempty" jsonschema:"Secret reference (op://, env:NAME or file:PATH) whose content is a header list in the OTEL_EXPORTER_OTLP_HEADERS format, key=value,key2=value2 with URL-encoded values. Merged with headers."`
	CaptureContent bool              `json:"capture_content,omitempty" jsonschema:"Record prompts, responses and tool input and output on spans. Off by default because they can hold secrets."`
}

// Listen configures the HTTP listener.
type Listen struct {
	Addr        string `json:"addr,omitempty" jsonschema:"Address to listen on, as host:port. Defaults to 127.0.0.1:7878."`
	IdleTimeout string `json:"idle_timeout,omitempty" jsonschema:"Close a session with no clients and no work after this long, as a Go duration. Defaults to 5m."`
	OpenTimeout string `json:"open_timeout,omitempty" jsonschema:"Give up opening a session after this long, as a Go duration. Defaults to 10s."`
	Heartbeat   string `json:"heartbeat,omitempty" jsonschema:"Send a heartbeat on an idle event stream this often, as a Go duration. Defaults to 20s."`
}

// Provider configures the model provider.
type Provider struct {
	Name     string `json:"name,omitempty" jsonschema:"Model provider. Empty uses anthropic."`
	Protocol string `json:"protocol,omitempty" jsonschema:"Wire protocol. Empty uses the provider's default."`
	Model    string `json:"model,omitempty" jsonschema:"Model name. Required for openrouter; for anthropic, empty uses the built-in default."`
	Effort   string `json:"effort,omitempty" jsonschema:"Reasoning effort for sessions. Empty uses the built-in default on anthropic-messages and the model's own default on openai-responses."`
	APIKey   string `json:"api_key,omitempty" jsonschema:"Where to read the provider's API key: an op:// 1Password reference, env:NAME or file:PATH. Never the key itself. Empty uses the provider's default credentials."`
}

// Git configures commits and git hosts.
type Git struct {
	Author string             `json:"author,omitempty" jsonschema:"The git user.name and user.email for commits in new sandboxed sessions, as NAME <EMAIL>."`
	Hosts  map[string]GitHost `json:"hosts,omitempty" jsonschema:"Git hosts by kind. A host with a token lets sandboxed sessions clone and push to it."`
}

// GitHost configures one git host with either a static token or a GitHub App.
type GitHost struct {
	Token string      `json:"token,omitempty" jsonschema:"Where to read a static token: an op:// 1Password reference, gh for the GitHub CLI's token, env:NAME or file:PATH. Never the token itself. Mutually exclusive with app."`
	App   *GitHostApp `json:"app,omitempty" jsonschema:"GitHub App credentials for gateway-managed short-lived installation tokens. Mutually exclusive with token."`
}

// GitHostApp configures a GitHub App installation.
type GitHostApp struct {
	ClientID       string `json:"client_id" jsonschema:"GitHub App client ID used as the JWT issuer."`
	InstallationID int64  `json:"installation_id,omitempty" jsonschema:"Positive GitHub App installation ID. Empty discovers the installation per repository."`
	PrivateKey     string `json:"private_key" jsonschema:"Where to read the GitHub App RSA PEM private key: an op:// 1Password reference, env:NAME or file:PATH. Never the key itself."`
}

// Environment pairs a worker image with network grants.
type Environment struct {
	Image  string   `json:"image" jsonschema:"The worker image sessions in this environment run."`
	Grants []string `json:"grants,omitempty" jsonschema:"Network grants for sessions in this environment."`
	// The jsonschema tag can't hold a comma, so the description reads without one.
	Approvals string `json:"approvals,omitempty" jsonschema:"Whether tool calls in sandboxed sessions of this environment ask for approval: ask (default) or auto. With auto calls run without approval; hooks still apply."`
}

// Source says where a setting's value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFlag    Source = "flag"
	SourceLegacy  Source = "environments.json"
)

// Warning is a note about the settings that doesn't stop the server.
type Warning struct {
	Field   string
	Message string
}

// Settings is the effective configuration: defaults, then the file, then flags.
type Settings struct {
	Addr        string
	IdleTimeout time.Duration
	OpenTimeout time.Duration
	Heartbeat   time.Duration
	Provider    string
	Protocol    string
	Model       string
	Effort      string
	// APIKey is a secret reference; empty leaves the provider's own credential lookup in charge.
	APIKey    string
	GitAuthor string
	// GitHosts maps a host kind to its static token's secret reference.
	GitHosts map[string]string
	// GitHostApps maps a host kind to its GitHub App settings.
	GitHostApps map[string]GitHostApp
	// OTel* are the trace exporter settings; an empty endpoint means tracing is off.
	OTelEndpoint       string
	OTelProtocol       string
	OTelCaptureContent bool
	// OTelHeaders maps a header name to its secret reference.
	OTelHeaders map[string]string
	// OTelHeadersFrom is the secret reference of an OTLP header list.
	OTelHeadersFrom string
	Environments    map[string]commands.Environment

	// Sources is keyed by field path, such as listen.addr or environments.go.
	Sources  map[string]Source
	Warnings []Warning

	errs     []error
	identity sandbox.GitIdentity
	// file is the config file's name, such as config.yaml; label is how errors name it.
	file      Source
	fileLabel string
}

func defaults() *Settings {
	s := &Settings{
		Addr:         "127.0.0.1:7878",
		IdleTimeout:  5 * time.Minute,
		OpenTimeout:  10 * time.Second,
		Heartbeat:    20 * time.Second,
		GitAuthor:    DefaultGitAuthor,
		GitHosts:     map[string]string{},
		GitHostApps:  map[string]GitHostApp{},
		OTelHeaders:  map[string]string{},
		Environments: map[string]commands.Environment{},
		Sources:      map[string]Source{},
		file:         "config.json",
		fileLabel:    "~/.durable/config.json",
	}
	for _, f := range []string{"listen.addr", "listen.idle_timeout", "listen.open_timeout", "listen.heartbeat", "provider.name", "provider.protocol", "provider.model", "provider.effort", "provider.api_key", "git.author", "otel.endpoint", "otel.protocol", "otel.capture_content"} {
		s.Sources[f] = SourceDefault
	}
	return s
}

// ParseGitAuthor splits NAME <EMAIL>.
func ParseGitAuthor(v string) (sandbox.GitIdentity, error) {
	a, err := mail.ParseAddress(v)
	if err != nil || a.Name == "" || a.Address == "" {
		return sandbox.GitIdentity{}, errors.New("must be NAME <EMAIL>")
	}
	return sandbox.GitIdentity{Name: a.Name, Email: a.Address}, nil
}

// Load reads the config file and the legacy environments file under home/.durable. A missing file means the defaults.
func Load(home string) (*Settings, error) {
	s := defaults()
	f, err := userfile.Load(home, BaseName)
	if err != nil {
		return nil, err
	}
	s.file = Source(f.Name)
	s.fileLabel = f.Label
	cfg, err := s.readFile(f.Data)
	if err != nil {
		return nil, err
	}
	if err := s.fromFile(cfg); err != nil {
		return nil, err
	}
	if !s.HasGitHost(github.Kind) && strings.TrimSpace(os.Getenv(githubTokenEnv)) != "" {
		// A variable alone configures GitHub; GITHUB_TOKEN and GH_TOKEN are never read, because they belong to other tools.
		s.GitHosts[github.Kind] = "env:" + githubTokenEnv
		s.Sources["git.hosts.github.token"] = SourceDefault
	}
	if err := s.loadLegacy(filepath.Join(home, ".durable", "environments.json")); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Settings) readFile(b []byte) (Config, error) {
	if b == nil {
		return Config{}, nil
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return Config{}, fmt.Errorf("failed to parse %s: %w", s.fileLabel, err)
	}
	if err := validateDocument(doc); err != nil {
		return Config{}, fmt.Errorf("invalid %s: %w", s.fileLabel, err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("failed to decode %s: %w", s.fileLabel, err)
	}
	return cfg, nil
}

func (s *Settings) fromFile(c Config) error {
	for _, d := range []struct {
		field, v string
		to       *time.Duration
	}{
		{"listen.idle_timeout", c.Listen.IdleTimeout, &s.IdleTimeout},
		{"listen.open_timeout", c.Listen.OpenTimeout, &s.OpenTimeout},
		{"listen.heartbeat", c.Listen.Heartbeat, &s.Heartbeat},
	} {
		if d.v == "" {
			continue
		}
		v, err := time.ParseDuration(d.v)
		if err != nil {
			return fmt.Errorf("invalid %s: %s: %w", s.fileLabel, d.field, err)
		}
		*d.to = v
		s.Sources[d.field] = s.file
	}
	for _, v := range []struct {
		field, in string
		to        *string
	}{
		{"listen.addr", c.Listen.Addr, &s.Addr},
		{"provider.name", c.Provider.Name, &s.Provider},
		{"provider.protocol", c.Provider.Protocol, &s.Protocol},
		{"provider.model", c.Provider.Model, &s.Model},
		{"provider.effort", c.Provider.Effort, &s.Effort},
		{"provider.api_key", c.Provider.APIKey, &s.APIKey},
		{"git.author", c.Git.Author, &s.GitAuthor},
	} {
		if v.in != "" {
			*v.to = v.in
			s.Sources[v.field] = s.file
		}
	}
	for kind, h := range c.Git.Hosts {
		if h.App != nil {
			s.GitHostApps[kind] = *h.App
			s.Sources["git.hosts."+kind+".app.client_id"] = s.file
			if h.App.InstallationID > 0 {
				s.Sources["git.hosts."+kind+".app.installation_id"] = s.file
			} else {
				s.Sources["git.hosts."+kind+".app.installation_id"] = SourceDefault
			}
			s.Sources["git.hosts."+kind+".app.private_key"] = s.file
			continue
		}
		s.GitHosts[kind] = h.Token
		s.Sources["git.hosts."+kind+".token"] = s.file
	}
	if c.OTel.Endpoint != "" {
		s.OTelEndpoint = c.OTel.Endpoint
		s.Sources["otel.endpoint"] = s.file
	}
	if c.OTel.Protocol != "" {
		s.OTelProtocol = c.OTel.Protocol
		s.Sources["otel.protocol"] = s.file
	}
	if c.OTel.CaptureContent {
		s.OTelCaptureContent = true
		s.Sources["otel.capture_content"] = s.file
	}
	for name, ref := range c.OTel.Headers {
		s.OTelHeaders[name] = ref
		s.Sources["otel.headers."+name] = s.file
	}
	if c.OTel.HeadersFrom != "" {
		s.OTelHeadersFrom = c.OTel.HeadersFrom
		s.Sources["otel.headers_from"] = s.file
	}
	for name, e := range c.Environments {
		s.Environments[name] = commands.Environment{Name: name, Image: e.Image, Grants: e.Grants, Approvals: e.Approvals}
		s.Sources["environments."+name] = s.file
	}
	return nil
}

func (s *Settings) loadLegacy(path string) error {
	envs, err := commands.LoadEnvironments(path)
	if err != nil {
		return fmt.Errorf("failed to load %s: %w", legacyLabel, err)
	}
	if len(envs) == 0 {
		return nil
	}
	if len(s.Environments) > 0 {
		s.errs = append(s.errs, fmt.Errorf("%s and %s both define environments; move them into %s and delete %s", s.fileLabel, legacyLabel, s.fileLabel, filepath.Base(path)))
		return nil
	}
	for name, e := range envs {
		s.Environments[name] = e
		s.Sources["environments."+name] = SourceLegacy
	}
	s.warn("environments", fmt.Sprintf("%s is deprecated; move its contents under \"environments\" in %s and delete it", legacyLabel, s.fileLabel))
	return nil
}

func (s *Settings) warn(field, msg string) {
	s.Warnings = append(s.Warnings, Warning{Field: field, Message: msg})
}

// Overrides are the command line flags; the zero value of each means it wasn't given.
type Overrides struct {
	Addr         string
	IdleTimeout  time.Duration
	OpenTimeout  time.Duration
	Heartbeat    time.Duration
	Provider     string
	Protocol     string
	Model        string
	Effort       string
	SandboxImage string
	GitHostFlags []string
	GitAuthor    string
	// APIKeyFlag names the flag that supplied the API key, or is empty.
	APIKeyFlag string
}

// override records that flag replaced field, and warns when it replaced a value from the file. It never names the value.
func (s *Settings) override(field, flag string) {
	if s.Sources[field] == s.file {
		s.warn(field, fmt.Sprintf("%s overrides %s from %s", flag, field, s.fileLabel))
	}
	s.Sources[field] = SourceFlag
}

// label names the setting the way the user set it, for errors.
func (s *Settings) label(field, flag string) string {
	if s.Sources[field] == SourceFlag {
		return flag
	}
	return field
}

// Apply layers the flags over the file. Problems with a flag are reported by Validate.
func (s *Settings) Apply(o Overrides) {
	if o.Addr != "" {
		s.override("listen.addr", "--addr")
		s.Addr = o.Addr
	}
	for _, d := range []struct {
		field, flag string
		v           time.Duration
		to          *time.Duration
	}{
		{"listen.idle_timeout", "--idle-timeout", o.IdleTimeout, &s.IdleTimeout},
		{"listen.open_timeout", "--open-timeout", o.OpenTimeout, &s.OpenTimeout},
		{"listen.heartbeat", "--heartbeat", o.Heartbeat, &s.Heartbeat},
	} {
		if d.v != 0 {
			s.override(d.field, d.flag)
			*d.to = d.v
		}
	}
	if o.Provider != "" && providerName(o.Provider) != providerName(s.Provider) {
		s.dropForProvider(o.Provider)
	}
	for _, v := range []struct {
		field, flag, in string
		to              *string
	}{
		{"provider.name", "--provider", o.Provider, &s.Provider},
		{"provider.protocol", "--protocol", o.Protocol, &s.Protocol},
		{"provider.model", "--model", o.Model, &s.Model},
		{"provider.effort", "--effort", o.Effort, &s.Effort},
		{"git.author", "--git-author", o.GitAuthor, &s.GitAuthor},
	} {
		if v.in != "" {
			s.override(v.field, v.flag)
			*v.to = v.in
		}
	}
	if o.APIKeyFlag != "" {
		s.override("provider.api_key", o.APIKeyFlag)
		s.APIKey = ""
	}
	s.applyGitHostFlags(o.GitHostFlags)
	s.applySandboxImage(o.SandboxImage)
}

// providerName treats empty as anthropic, the default provider.
func providerName(p string) string {
	if p == "" {
		return "anthropic"
	}
	return p
}

// dropForProvider clears the file's protocol, model, effort and key, which belong to the provider --provider replaces.
func (s *Settings) dropForProvider(provider string) {
	for _, d := range []struct {
		field string
		to    *string
	}{
		{"provider.protocol", &s.Protocol},
		{"provider.model", &s.Model},
		{"provider.effort", &s.Effort},
		{"provider.api_key", &s.APIKey},
	} {
		if s.Sources[d.field] != s.file {
			continue
		}
		*d.to = ""
		s.Sources[d.field] = SourceDefault
		s.warn(d.field, fmt.Sprintf("--provider %s ignores %s from %s, which is for another provider", provider, d.field, s.fileLabel))
	}
}

// applyGitHostFlags never echoes a flag value, because a token pasted there by mistake must not reach the log.
func (s *Settings) applyGitHostFlags(flags []string) {
	seen := map[string]bool{}
	for _, f := range flags {
		kind, value, ok := strings.Cut(f, "=")
		switch {
		case !ok || value == "":
			s.errs = append(s.errs, errors.New("--git-host-token must be <kind>=<source>, where source is an op:// reference, gh, env:NAME or file:PATH"))
			continue
		case !slices.Contains(GitHostKinds, kind):
			s.errs = append(s.errs, fmt.Errorf("unsupported git host kind in --git-host-token; supported kinds: %s", strings.Join(GitHostKinds, ", ")))
			continue
		case seen[kind]:
			s.errs = append(s.errs, fmt.Errorf("--git-host-token given more than once for %s", kind))
			continue
		case !secretRefRE.MatchString(value):
			s.errs = append(s.errs, fmt.Errorf("--git-host-token %s must be an op:// secret reference, gh, env:NAME or file:PATH", kind))
			continue
		}
		seen[kind] = true
		if _, hadApp := s.GitHostApps[kind]; hadApp {
			delete(s.GitHostApps, kind)
			delete(s.Sources, "git.hosts."+kind+".app.client_id")
			delete(s.Sources, "git.hosts."+kind+".app.installation_id")
			delete(s.Sources, "git.hosts."+kind+".app.private_key")
			s.warn("git.hosts."+kind+".app", fmt.Sprintf("--git-host-token %s overrides git.hosts.%s.app from %s", kind, kind, s.fileLabel))
		}
		s.override("git.hosts."+kind+".token", "--git-host-token "+kind)
		s.GitHosts[kind] = value
	}
}

func (s *Settings) applySandboxImage(image string) {
	if image == "" {
		return
	}
	if _, ok := s.Environments[commands.DefaultEnvironment]; ok {
		where := s.fileLabel
		if s.Sources["environments."+commands.DefaultEnvironment] == SourceLegacy {
			where = legacyLabel
		}
		s.errs = append(s.errs, fmt.Errorf("--sandbox-image defines the %q environment, which %s also defines", commands.DefaultEnvironment, where))
		return
	}
	s.Environments[commands.DefaultEnvironment] = commands.Environment{Name: commands.DefaultEnvironment, Image: image}
	s.Sources["environments."+commands.DefaultEnvironment] = SourceFlag
}

// Validate reports every problem the settings have, joined.
func (s *Settings) Validate() error {
	errs := slices.Clone(s.errs)
	if err := commands.CheckProtocol(s.Provider, s.Protocol); err != nil {
		errs = append(errs, fmt.Errorf("%s and %s: %w", s.label("provider.name", "--provider"), s.label("provider.protocol", "--protocol"), err))
	}
	if s.Provider == commands.ProviderOpenRouter && s.Model == "" {
		errs = append(errs, fmt.Errorf("provider openrouter needs a model; set provider.model in %s or pass --model", s.fileLabel))
	}
	errs = append(errs, s.validateGit()...)
	errs = append(errs, s.validateOTel()...)
	errs = append(errs, s.validateEnvironments()...)
	return errors.Join(errs...)
}

func (s *Settings) validateGit() []error {
	var errs []error
	id, err := ParseGitAuthor(s.GitAuthor)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s %q %w", s.label("git.author", "--git-author"), s.GitAuthor, err))
	}
	s.identity = id
	if len(s.GitHosts) > 0 && len(s.Environments) == 0 {
		errs = append(errs, errors.New("a git host token needs --sandbox-image or an environment, because repositories are only reachable from a sandbox"))
	}
	if len(s.GitHostApps) > 0 && len(s.Environments) == 0 {
		errs = append(errs, errors.New("a git host app needs --sandbox-image or an environment, because repositories are only reachable from a sandbox"))
	}
	for _, kind := range slices.Sorted(maps.Keys(s.GitHostApps)) {
		app := s.GitHostApps[kind]
		if strings.TrimSpace(app.ClientID) == "" {
			errs = append(errs, fmt.Errorf("git.hosts.%s.app.client_id is required", kind))
		}
		if app.InstallationID < 0 {
			errs = append(errs, fmt.Errorf("git.hosts.%s.app.installation_id must be positive", kind))
		}
	}
	return errs
}

func (s *Settings) validateEnvironments() []error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(s.Environments)) {
		e := s.Environments[name]
		if strings.TrimSpace(e.Image) == "" {
			errs = append(errs, fmt.Errorf("environment %q: image is required", name))
		}
		if slices.Contains(e.Grants, "mise-resolve") && !slices.Contains(e.Grants, "mise") {
			errs = append(errs, fmt.Errorf("environment %q: grant \"mise-resolve\" needs \"mise\", which downloads what it resolves", name))
		}
		if slices.Contains(e.Grants, "npm-registry") && !slices.Contains(e.Grants, "mise") {
			s.warn("environments."+name, fmt.Sprintf("environment %q names the npm-registry grant, which covers bun installed by mise, but not the mise grant that installs it", name))
		}
		if !s.HasGitHost(github.Kind) && slices.Contains(e.Grants, "github-api") {
			s.warn("environments."+name, fmt.Sprintf("environment %q names the github-api grant, which never applies without a github git host token or app", name))
		}
		if e.Approvals == commands.ApprovalsAuto && slices.Contains(e.Grants, "github-api") {
			s.warn("environments."+name, fmt.Sprintf("environment %q auto-approves tool calls and has the github-api grant, so its sessions push, open PRs and comment without approval", name))
		}
	}
	return errs
}

func (s *Settings) validateOTel() []error {
	if s.OTelEndpoint == "" {
		if s.OTelProtocol != "" || s.OTelCaptureContent || len(s.OTelHeaders) > 0 || s.OTelHeadersFrom != "" {
			return []error{errors.New("otel.protocol, otel.headers, otel.headers_from and otel.capture_content need otel.endpoint")}
		}
		return nil
	}
	var errs []error
	if u, err := url.Parse(s.OTelEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("otel.endpoint must be an absolute http or https URL with a host"))
	}
	if s.OTelProtocol != "" && !slices.Contains(OTelProtocols, s.OTelProtocol) {
		errs = append(errs, fmt.Errorf("otel.protocol must be one of %s", strings.Join(OTelProtocols, ", ")))
	}
	for _, name := range slices.Sorted(maps.Keys(s.OTelHeaders)) {
		if !httpguts.ValidHeaderFieldName(name) {
			errs = append(errs, fmt.Errorf("otel.headers: header %q has an invalid name", name))
		}
	}
	return errs
}

// OTelSettings is the trace exporter configuration with the protocol default applied.
type OTelSettings struct {
	Endpoint       string
	Protocol       string
	CaptureContent bool
	Headers        map[string]creds.Source
	// HeadersFrom is nil when unset.
	HeadersFrom *creds.Source
}

// OTel returns the trace exporter settings; Endpoint is empty when tracing is off.
func (s *Settings) OTel() OTelSettings {
	o := OTelSettings{Endpoint: s.OTelEndpoint, Protocol: s.OTelProtocol, CaptureContent: s.OTelCaptureContent, Headers: make(map[string]creds.Source, len(s.OTelHeaders))}
	if o.Protocol == "" {
		o.Protocol = OTelProtocolHTTP
	}
	for name, ref := range s.OTelHeaders {
		o.Headers[name] = creds.Source{Flag: "otel.headers." + name, Value: ref}
	}
	if s.OTelHeadersFrom != "" {
		o.HeadersFrom = &creds.Source{Flag: "otel.headers_from", Value: s.OTelHeadersFrom}
	}
	return o
}

// GitIdentity is the parsed git author; it is valid after Validate succeeds.
func (s *Settings) GitIdentity() sandbox.GitIdentity { return s.identity }

// HasGitHost reports whether kind has either a static token or a GitHub App configured.
func (s *Settings) HasGitHost(kind string) bool {
	if _, ok := s.GitHosts[kind]; ok {
		return true
	}
	_, ok := s.GitHostApps[kind]
	return ok
}

// GitHostAppSource is a resolved GitHub App configuration with its private key secret source.
type GitHostAppSource struct {
	ClientID       string
	InstallationID int64
	PrivateKey     creds.Source
}

// GitHostSources returns each git host's static token source.
func (s *Settings) GitHostSources() map[string]creds.Source {
	out := make(map[string]creds.Source, len(s.GitHosts))
	for kind, ref := range s.GitHosts {
		out[kind] = creds.Source{Flag: s.label("git.hosts."+kind+".token", "--git-host-token "+kind), Value: ref}
	}
	return out
}

// GitHostAppSources returns each git host's GitHub App configuration.
func (s *Settings) GitHostAppSources() map[string]GitHostAppSource {
	out := make(map[string]GitHostAppSource, len(s.GitHostApps))
	for kind, app := range s.GitHostApps {
		out[kind] = GitHostAppSource{
			ClientID:       app.ClientID,
			InstallationID: app.InstallationID,
			PrivateKey:     creds.Source{Flag: "git.hosts." + kind + ".app.private_key", Value: app.PrivateKey},
		}
	}
	return out
}

// APIKeySource returns where the file says the provider key lives; Value is empty when it doesn't.
func (s *Settings) APIKeySource() creds.Source {
	return creds.Source{Flag: "provider.api_key", Value: s.APIKey}
}
