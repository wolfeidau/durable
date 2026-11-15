package userfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

type decodeError struct {
	msg string
	err error
}

func (e *decodeError) Error() string { return e.msg }
func (e *decodeError) Unwrap() error { return e.err }

// DescribeDecodeError rewrites a JSON type mismatch as a plain message for a config author, and passes other errors through.
func DescribeDecodeError(err error) error {
	ute, ok := errors.AsType[*json.UnmarshalTypeError](err)
	if !ok {
		return err
	}
	want, hint := "value", ""
	if ute.Type != nil {
		switch ute.Type.Kind() { //nolint:exhaustive // the other kinds read as a value
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			want = "number"
		case reflect.Bool:
			want = "boolean"
		case reflect.String:
			want = "string"
		case reflect.Slice, reflect.Array:
			want = "list"
		case reflect.Map, reflect.Struct:
			want = "map"
		}
	}
	if want == "number" || want == "boolean" {
		hint = "; in YAML write numbers as plain decimals (no 0x, leading zeros or underscores) and booleans as true or false"
	}
	got := ute.Value
	switch got {
	case "bool":
		got = "boolean"
	case "array":
		got = "list"
	case "object":
		got = "map"
	}
	msg := fmt.Sprintf("must be a %s, got %s%s", want, got, hint)
	if field := strings.TrimLeft(ute.Field, "."); field != "" {
		msg = field + " " + msg
	}
	return &decodeError{msg: msg, err: err}
}
