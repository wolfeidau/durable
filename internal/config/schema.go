package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/wolfeidau/durable/internal/commands"
)

// SchemaID is where the committed schema is published.
const SchemaID = "https://raw.githubusercontent.com/wolfeidau/durable/main/schema/durable-server.schema.json"

func enum(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Schema builds the JSON Schema of Config; the struct tags carry the descriptions and the rest is added here.
func Schema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[Config](nil)
	if err != nil {
		return nil, fmt.Errorf("failed to infer the config schema: %w", err)
	}
	s.Schema = "https://json-schema.org/draft/2020-12/schema"
	s.ID = SchemaID
	s.Title = "durable-server config"
	s.Description = "Settings for durable-server, read from ~/.durable/config.json. Command line flags override them."

	listen := s.Properties["listen"]
	for _, name := range []string{"idle_timeout", "open_timeout", "heartbeat"} {
		listen.Properties[name].Pattern = durationPattern
	}

	provider := s.Properties["provider"]
	provider.Properties["name"].Enum = enum(commands.ProviderNames())
	provider.Properties["protocol"].Enum = enum(commands.ProtocolNames())
	provider.Properties["api_key"].Pattern = apiKeyRefPattern

	hosts := s.Properties["git"].Properties["hosts"]
	hosts.PropertyNames = &jsonschema.Schema{Enum: enum(GitHostKinds)}
	hosts.AdditionalProperties.Properties["token"].Pattern = secretRefPattern

	envs := s.Properties["environments"]
	env := envs.AdditionalProperties
	env.Properties["image"].MinLength = new(1)
	grants := env.Properties["grants"]
	grants.Types, grants.Type = nil, "array"
	grants.Items.Enum = enum(commands.GrantNames())
	grants.UniqueItems = true
	env.Properties["approvals"].Enum = enum([]string{commands.ApprovalsAsk, commands.ApprovalsAuto})
	envs.PatternProperties = map[string]*jsonschema.Schema{commands.EnvironmentNamePattern: env}
	falseSchema := &jsonschema.Schema{Not: &jsonschema.Schema{}}
	envs.AdditionalProperties = falseSchema
	return s, nil
}

// SchemaJSON is the committed schema file's content.
func SchemaJSON() ([]byte, error) {
	s, err := Schema()
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to encode the config schema: %w", err)
	}
	return append(b, '\n'), nil
}

var resolved = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	s, err := Schema()
	if err != nil {
		return nil, err
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the config schema: %w", err)
	}
	return r, nil
})

// validateDocument checks the decoded file against the schema and returns the first error.
func validateDocument(doc any) error {
	if err := checkSecrets(doc); err != nil {
		return err
	}
	r, err := resolved()
	if err != nil {
		return err
	}
	if err := r.Validate(doc); err != nil {
		return trimPath(err)
	}
	return nil
}

// redact rewrites the library's message without the value it quotes, because that value can be a pasted secret.
func redact(problem string) string {
	keyword, rest, _ := strings.Cut(problem, ": ")
	after := func(sep string) string {
		_, tail, _ := strings.CutLast(rest, sep)
		return tail
	}
	switch keyword {
	case "required", "uniqueItems", "minItems", "maxItems", "minProperties", "maxProperties":
		return problem
	case "type":
		if t := after(" has type "); t != "" {
			return "wrong type: has type " + t
		}
	case "enum":
		return "must be one of " + after(" does not equal any of: ")
	case "pattern":
		return "does not match " + after(" does not match regular expression ")
	case "minLength":
		return "must not be empty"
	}
	if strings.HasPrefix(problem, "unexpected additional properties ") {
		return problem
	}
	return "does not match the schema"
}

// trimPath turns the library's chain of "validating <schema pointer>:" wrappers into "path: problem".
func trimPath(err error) error {
	_, last, found := strings.CutLast(err.Error(), "validating ")
	if !found {
		return errors.New("does not match the schema")
	}
	ptr, problem, ok := strings.Cut(last, ": ")
	problem = redact(problem)
	if ptr == SchemaID {
		return errors.New(problem)
	}
	if !ok || !strings.HasPrefix(ptr, "/") {
		return errors.New(problem)
	}
	var path strings.Builder
	segs := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	for j := 0; j < len(segs); j++ {
		switch segs[j] {
		case "properties":
			if j++; j < len(segs) {
				path.WriteString("." + segs[j])
			}
		case "patternProperties":
			j++
			path.WriteString(".<name>")
		case "additionalProperties":
			path.WriteString(".<name>")
		case "items":
			path.WriteString("[]")
		}
	}
	return errors.New(strings.TrimPrefix(path.String(), ".") + ": " + problem)
}

// checkSecrets runs before the schema, because the library's messages quote the value that failed.
func checkSecrets(doc any) error {
	root, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	if p, ok := root["provider"].(map[string]any); ok {
		if v, ok := p["api_key"]; ok {
			if err := checkRef("provider.api_key", v, apiKeyRefRE.MatchString, "an op:// secret reference or env:NAME"); err != nil {
				return err
			}
		}
	}
	g, _ := root["git"].(map[string]any)
	hosts, _ := g["hosts"].(map[string]any)
	for kind, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := host["token"]; ok {
			if err := checkRef("git.hosts."+kind+".token", v, secretRefRE.MatchString, "an op:// secret reference, gh or env:NAME"); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRef(field string, v any, ok func(string) bool, want string) error {
	if s, isString := v.(string); isString && ok(s) {
		return nil
	}
	return errors.New(field + " must be " + want + "; the file holds references, never secrets")
}
