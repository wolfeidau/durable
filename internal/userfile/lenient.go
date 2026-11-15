package userfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// StringList decodes a JSON array of strings, numbers and booleans, so YAML like `args: [--port, 8080]` works.
func StringList(field string, raw json.RawMessage) ([]string, error) {
	// YAML `args:` with every entry commented out is null, and JSON null always meant empty.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("%s must be a list of strings, numbers or booleans", field)
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, err := scalarText(it)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] %w", field, i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// StringMap decodes a JSON object of strings, numbers and booleans, so YAML like `env: {PORT: 8080}` works.
func StringMap(field string, raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("%s must be a map of strings, numbers or booleans", field)
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(items))
	for _, k := range keys {
		s, err := scalarText(items[k])
		if err != nil {
			return nil, fmt.Errorf("%s.%s %w", field, k, err)
		}
		out[k] = s
	}
	return out, nil
}

// scalarText returns a string as is and a number or boolean as its literal text.
func scalarText(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case float64, bool:
		return string(raw), nil
	}
	return "", errors.New("must be a string, number or boolean")
}
