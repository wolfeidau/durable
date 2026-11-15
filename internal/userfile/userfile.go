// Package userfile finds and reads the user's config files, which may be JSON or YAML.
package userfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

var exts = []string{".json", ".yaml", ".yml"}

// File is a user config file as JSON. Data is nil when no file exists.
type File struct {
	Data []byte
	// Name is the file's base name and Label is how errors show it, never with the home directory.
	Name, Label string
}

// Load reads the one of home/.durable/base.json, base.yaml and base.yml that exists, converting YAML so one decoder serves both.
// A missing file returns a File with nil Data, so the caller uses its defaults.
func Load(home, base string) (File, error) {
	dir := filepath.Join(home, ".durable")
	var found []string
	for _, e := range exts {
		if _, err := os.Stat(filepath.Join(dir, base+e)); err == nil {
			found = append(found, base+e)
		}
	}
	if len(found) > 1 {
		return File{}, fmt.Errorf("~/.durable: both %s exist; keep one", strings.Join(found, " and "))
	}
	f := File{Name: base + ".json"}
	if len(found) == 1 {
		f.Name = found[0]
	}
	f.Label = "~/.durable/" + f.Name
	b, err := os.ReadFile(filepath.Join(dir, f.Name)) //nolint:gosec // the user's own config path
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		// The path error would print the home directory.
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return File{}, fmt.Errorf("failed to read %s: %w", f.Label, err)
	}
	if filepath.Ext(f.Name) != ".json" {
		if b, err = yamlToJSON(b); err != nil {
			return File{}, fmt.Errorf("failed to parse %s: %w", f.Label, err)
		}
	}
	f.Data = b
	return f, nil
}

func yamlToJSON(b []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return []byte("{}"), nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("more than one YAML document; keep one")
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null" {
		return []byte("{}"), nil
	}
	v, err := convert(root, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// convert builds a value for json.Marshal, keeping scalars as their YAML text where the type allows.
func convert(n *yaml.Node, path string) (any, error) {
	switch n.Kind {
	case yaml.AliasNode:
		return convert(n.Alias, path)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, c := range n.Content {
			v, err := convert(c, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		return convertMap(n, path)
	case yaml.DocumentNode, yaml.ScalarNode:
	}
	return convertScalar(n, path)
}

func convertMap(n *yaml.Node, path string) (map[string]any, error) {
	out := map[string]any{}
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
			merges = append(merges, val)
			continue
		}
		p := join(path, k.Value)
		if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
			return nil, fmt.Errorf("key %s is not a string", p)
		}
		if _, dup := out[k.Value]; dup {
			return nil, fmt.Errorf("key %s is duplicated", p)
		}
		v, err := convert(val, p)
		if err != nil {
			return nil, err
		}
		out[k.Value] = v
	}
	if err := mergeInto(out, merges, path); err != nil {
		return nil, err
	}
	return out, nil
}

// mergeInto adds merged keys without overriding the mapping's own, and an earlier merge wins over a later one.
func mergeInto(out map[string]any, merges []*yaml.Node, path string) error {
	for _, m := range merges {
		srcs := []*yaml.Node{m}
		if m.Kind == yaml.SequenceNode {
			srcs = m.Content
		}
		for _, src := range srcs {
			if src.Kind == yaml.AliasNode {
				src = src.Alias
			}
			if src.Kind != yaml.MappingNode {
				return fmt.Errorf("%s: merge key << needs a mapping", pathOrRoot(path))
			}
			v, err := convertMap(src, path)
			if err != nil {
				return err
			}
			for k, c := range v {
				if _, ok := out[k]; !ok {
					out[k] = c
				}
			}
		}
	}
	return nil
}

func convertScalar(n *yaml.Node, path string) (any, error) {
	switch tag := n.ShortTag(); tag {
	case "!!str", "!!timestamp":
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("%s: %w", pathOrRoot(path), err)
		}
		return b, nil
	case "!!int", "!!float":
		if json.Valid([]byte(n.Value)) {
			return json.Number(n.Value), nil
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s: %w", pathOrRoot(path), err)
		}
		if f, ok := v.(float64); ok {
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, fmt.Errorf("%s: %s cannot be represented in JSON", pathOrRoot(path), n.Value)
			}
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
		}
		return v, nil
	default:
		return nil, fmt.Errorf("%s: tag %s is not supported", pathOrRoot(path), tag)
	}
}

func pathOrRoot(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}
