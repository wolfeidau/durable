// Package userfile finds and reads the user's config files, which may be JSON or YAML.
package userfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

var exts = []string{".json", ".yaml", ".yml"}

// Find returns the one of base.json, base.yaml and base.yml in dir. None means the base.json path, so a missing file means the defaults.
func Find(dir, base string) (string, error) {
	var found []string
	for _, e := range exts {
		if _, err := os.Stat(filepath.Join(dir, base+e)); err == nil {
			found = append(found, base+e)
		}
	}
	switch len(found) {
	case 0:
		return filepath.Join(dir, base+".json"), nil
	case 1:
		return filepath.Join(dir, found[0]), nil
	}
	return "", fmt.Errorf("~/%s has both %s; keep one", filepath.Base(dir), strings.Join(found, " and "))
}

// ReadJSON returns the file as JSON, converting YAML so one decoder serves both.
func ReadJSON(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the user's own config path
	if err != nil {
		return nil, err
	}
	if filepath.Ext(path) == ".json" {
		return b, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc any
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to parse %s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", filepath.Base(path), err)
		}
		return nil, fmt.Errorf("%s has more than one YAML document; keep one", filepath.Base(path))
	}
	if doc == nil {
		return []byte("{}"), nil
	}
	if err := checkKeys(doc, ""); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to convert %s to JSON: %w", filepath.Base(path), err)
	}
	return out, nil
}

func checkKeys(v any, path string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if err := checkKeys(c, join(path, k)); err != nil {
				return err
			}
		}
	case map[any]any:
		for k := range t {
			return fmt.Errorf("key %s is not a string", join(path, fmt.Sprint(k)))
		}
	case []any:
		for i, c := range t {
			if err := checkKeys(c, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}
