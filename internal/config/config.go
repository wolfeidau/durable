// Package config reads the durable-server settings from ~/.durable/config.json and the command line flags.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/creds"
	"github.com/wolfeidau/durable/internal/githost/github"
	"github.com/wolfeidau/durable/internal/sandbox"
)

const (
	// FileName is the config file inside ~/.durable.
	FileName = "config.json"

	configLabel = "~/.durable/config.json"
	legacyLabel = "~/.durable/environments.json"

	githubTokenEnv = "DURABLE_GITHUB_TOKEN" //nolint:gosec // a variable name, not a credential

	// DefaultGitAuthor is the commit identity of sandboxed sessions.
	DefaultGitAuthor = "durable <durable@localhost>"
)

// GitHostKinds lists the supported git hosts.
var GitHostKinds = []string{github.Kind}

// Secret reference patterns: a pasted value matches none of them.
//
//nolint:gosec // patterns, not credentials
const (
	secretRefPattern = `^(op://.+|gh|env:[A-Za-z_][A-Za-z0-9_]*)$`
	apiKeyRefPattern = `^(op://.+|env:[A-Za-z_][A-Za-z0-9_]*)$`
	durationPattern  = `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
)

var (
	secretRefRE = regexp.MustCompile(secretRefPattern)
	apiKeyRefRE = regexp.MustCompile(apiKeyRefPattern)
)

// Config is the shape of ~/.durable/config.json.
type Config struct {
	Schema       string                 `json:"$schema,omitempty" jsonschema:"URL of this schema, so editors can validate the file."`
	Listen       Listen                 `json:"listen,omitzero" jsonschema:"Where the server listens and how it times out clients."`
	Provider     Provider               `json:"provider,omitzero" jsonschema:"The model provider for new sessions. Existing sessions keep their pin."`
	Git          Git                    `json:"git,omitzero" jsonschema:"Git settings for sandboxed sessions."`
	Environments map[string]Environment `json:"environments,omitempty" jsonschema:"Sandbox environments, by name. A session picks one; default is used when it names none."`
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
	APIKey   string `json:"api_key,omitempty" jsonschema:"Where to read the provider's API key: an op:// 1Password reference or env:NAME. Never the key itself. Empty uses the provider's default credentials."`
}

// Git configures commits and git hosts.
type Git struct {
	Author string             `json:"author,omitempty" jsonschema:"The git user.name and user.email for commits in new sandboxed sessions, as NAME <EMAIL>."`
	Hosts  map[string]GitHost `json:"hosts,omitempty" jsonschema:"Git hosts by kind. A host with a token lets sandboxed sessions clone and push to it."`
}

// GitHost configures one git host.
type GitHost struct {
	Token string `json:"token" jsonschema:"Where to read the token: an op:// 1Password reference, gh for the GitHub CLI's token, or env:NAME. Never the token itself."`
}

// Environment pairs a worker image with network grants.
type Environment struct {
	Image  string   `json:"image" jsonschema:"The worker image sessions in this environment run."`
	Grants []string `json:"grants,omitempty" jsonschema:"Network grants for sessions in this environment."`
}

// Source says where a setting's value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "config.json"
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
	// APIKey is a secret reference; empty leaves the provider's own credential lookup in charge.
	APIKey    string
	GitAuthor string
	// GitHosts maps a host kind to its token's secret reference.
	GitHosts     map[string]string
	Environments map[string]commands.Environment

	// Sources is keyed by field path, such as listen.addr or environments.go.
	Sources  map[string]Source
	Warnings []Warning

	errs     []error
	identity sandbox.GitIdentity
}

func defaults() *Settings {
	s := &Settings{
		Addr:         "127.0.0.1:7878",
		IdleTimeout:  5 * time.Minute,
		OpenTimeout:  10 * time.Second,
		Heartbeat:    20 * time.Second,
		GitAuthor:    DefaultGitAuthor,
		GitHosts:     map[string]string{},
		Environments: map[string]commands.Environment{},
		Sources:      map[string]Source{},
	}
	for _, f := range []string{"listen.addr", "listen.idle_timeout", "listen.open_timeout", "listen.heartbeat", "provider.name", "provider.protocol", "provider.model", "provider.api_key", "git.author"} {
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
	cfg, err := readFile(filepath.Join(home, ".durable", FileName))
	if err != nil {
		return nil, err
	}
	if err := s.fromFile(cfg); err != nil {
		return nil, err
	}
	if _, ok := s.GitHosts[github.Kind]; !ok && strings.TrimSpace(os.Getenv(githubTokenEnv)) != "" {
		// A variable alone configures GitHub; GITHUB_TOKEN and GH_TOKEN are never read, because they belong to other tools.
		s.GitHosts[github.Kind] = "env:" + githubTokenEnv
		s.Sources["git.hosts.github.token"] = SourceDefault
	}
	if err := s.loadLegacy(filepath.Join(home, ".durable", "environments.json")); err != nil {
		return nil, err
	}
	return s, nil
}

func readFile(path string) (Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config path
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("failed to read %s: %w", configLabel, err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return Config{}, fmt.Errorf("failed to parse %s: %w", configLabel, err)
	}
	if err := validateDocument(doc); err != nil {
		return Config{}, fmt.Errorf("invalid %s: %w", configLabel, err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("failed to decode %s: %w", configLabel, err)
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
			return fmt.Errorf("invalid %s: %s: %w", configLabel, d.field, err)
		}
		*d.to = v
		s.Sources[d.field] = SourceFile
	}
	for _, v := range []struct {
		field, in string
		to        *string
	}{
		{"listen.addr", c.Listen.Addr, &s.Addr},
		{"provider.name", c.Provider.Name, &s.Provider},
		{"provider.protocol", c.Provider.Protocol, &s.Protocol},
		{"provider.model", c.Provider.Model, &s.Model},
		{"provider.api_key", c.Provider.APIKey, &s.APIKey},
		{"git.author", c.Git.Author, &s.GitAuthor},
	} {
		if v.in != "" {
			*v.to = v.in
			s.Sources[v.field] = SourceFile
		}
	}
	for kind, h := range c.Git.Hosts {
		s.GitHosts[kind] = h.Token
		s.Sources["git.hosts."+kind+".token"] = SourceFile
	}
	for name, e := range c.Environments {
		s.Environments[name] = commands.Environment{Name: name, Image: e.Image, Grants: e.Grants}
		s.Sources["environments."+name] = SourceFile
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
		s.errs = append(s.errs, fmt.Errorf("%s and %s both define environments; move them into %s and delete %s", configLabel, legacyLabel, FileName, filepath.Base(path)))
		return nil
	}
	for name, e := range envs {
		s.Environments[name] = e
		s.Sources["environments."+name] = SourceLegacy
	}
	s.warn("environments", fmt.Sprintf("%s is deprecated; move its contents under \"environments\" in %s and delete it", legacyLabel, configLabel))
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
	SandboxImage string
	GitHostFlags []string
	GitAuthor    string
	// APIKeyFlag names the flag that supplied the API key, or is empty.
	APIKeyFlag string
}

// override records that flag replaced field, and warns when it replaced a value from the file. It never names the value.
func (s *Settings) override(field, flag string) {
	if s.Sources[field] == SourceFile {
		s.warn(field, fmt.Sprintf("%s overrides %s from %s", flag, field, configLabel))
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

// dropForProvider clears the file's protocol, model and key, which belong to the provider --provider replaces.
func (s *Settings) dropForProvider(provider string) {
	for _, d := range []struct {
		field string
		to    *string
	}{
		{"provider.protocol", &s.Protocol},
		{"provider.model", &s.Model},
		{"provider.api_key", &s.APIKey},
	} {
		if s.Sources[d.field] != SourceFile {
			continue
		}
		*d.to = ""
		s.Sources[d.field] = SourceDefault
		s.warn(d.field, fmt.Sprintf("--provider %s ignores %s from %s, which is for another provider", provider, d.field, configLabel))
	}
}

// applyGitHostFlags never echoes a flag value, because a token pasted there by mistake must not reach the log.
func (s *Settings) applyGitHostFlags(flags []string) {
	seen := map[string]bool{}
	for _, f := range flags {
		kind, value, ok := strings.Cut(f, "=")
		switch {
		case !ok || value == "":
			s.errs = append(s.errs, errors.New("--git-host-token must be <kind>=<source>, where source is an op:// reference, gh or env:NAME"))
			continue
		case !slices.Contains(GitHostKinds, kind):
			s.errs = append(s.errs, fmt.Errorf("unsupported git host kind in --git-host-token; supported kinds: %s", strings.Join(GitHostKinds, ", ")))
			continue
		case seen[kind]:
			s.errs = append(s.errs, fmt.Errorf("--git-host-token given more than once for %s", kind))
			continue
		case !secretRefRE.MatchString(value):
			s.errs = append(s.errs, fmt.Errorf("--git-host-token %s must be an op:// secret reference, gh or env:NAME", kind))
			continue
		}
		seen[kind] = true
		s.override("git.hosts."+kind+".token", "--git-host-token "+kind)
		s.GitHosts[kind] = value
	}
}

func (s *Settings) applySandboxImage(image string) {
	if image == "" {
		return
	}
	if _, ok := s.Environments[commands.DefaultEnvironment]; ok {
		where := configLabel
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
		errs = append(errs, fmt.Errorf("provider openrouter needs a model; set provider.model in %s or pass --model", configLabel))
	}
	id, err := ParseGitAuthor(s.GitAuthor)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s %q %w", s.label("git.author", "--git-author"), s.GitAuthor, err))
	}
	s.identity = id
	if len(s.GitHosts) > 0 && len(s.Environments) == 0 {
		errs = append(errs, errors.New("a git host token needs --sandbox-image or an environment, because repositories are only reachable from a sandbox"))
	}
	for _, name := range slices.Sorted(maps.Keys(s.Environments)) {
		if strings.TrimSpace(s.Environments[name].Image) == "" {
			errs = append(errs, fmt.Errorf("environment %q: image is required", name))
		}
	}
	return errors.Join(errs...)
}

// GitIdentity is the parsed git author; it is valid after Validate succeeds.
func (s *Settings) GitIdentity() sandbox.GitIdentity { return s.identity }

// GitHostSources returns each git host's token source.
func (s *Settings) GitHostSources() map[string]creds.Source {
	out := make(map[string]creds.Source, len(s.GitHosts))
	for kind, ref := range s.GitHosts {
		out[kind] = creds.Source{Flag: s.label("git.hosts."+kind+".token", "--git-host-token "+kind), Value: ref}
	}
	return out
}

// APIKeySource returns where the file says the provider key lives; Value is empty when it doesn't.
func (s *Settings) APIKeySource() creds.Source {
	return creds.Source{Flag: "provider.api_key", Value: s.APIKey}
}
