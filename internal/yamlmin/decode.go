package yamlmin

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var durationType = reflect.TypeOf(time.Duration(0))

// Unmarshal parses a YAML document and stores the result in the value pointed
// to by out, which must be a non-nil pointer to a struct.
//
// Keys present in the document but absent from the struct are ignored, which is
// what lets an older agent read a config written for a newer one — and is the
// behaviour the previous library had, so upgrades keep working the same way.
func Unmarshal(data []byte, out any) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("yamlmin: Unmarshal needs a non-nil pointer, got %T", out)
	}
	root, err := parse(data)
	if err != nil {
		return err
	}
	return assign(root, rv.Elem(), "")
}

// assign writes a parsed node into a Go value. path is the dotted key path so
// far, used only for error messages — a config error that does not say which
// key it is about costs more time than the parse itself.
func assign(n *node, v reflect.Value, path string) error {
	// An explicitly empty value leaves the Go zero value in place, matching
	// "key:" with nothing after it meaning "not set".
	if n == nil || (n.kind == kindScalar && n.scalar == "") {
		return nil
	}

	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return assign(n, v.Elem(), path)
	}

	// time.Duration is an int64, so it has to be handled before the integer
	// case or "15s" would be rejected as a malformed number.
	if v.Type() == durationType {
		return assignDuration(n, v, path)
	}

	switch v.Kind() {
	case reflect.Struct:
		return assignStruct(n, v, path)
	case reflect.Slice:
		return assignSlice(n, v, path)
	case reflect.Map:
		return assignMap(n, v, path)
	case reflect.String:
		if n.kind != kindScalar {
			return typeErr(n, path, "a string")
		}
		v.SetString(n.scalar)
		return nil
	case reflect.Bool:
		if n.kind != kindScalar {
			return typeErr(n, path, "a boolean")
		}
		b, err := parseBool(n.scalar)
		if err != nil {
			return fmt.Errorf("line %d: %s: %w", n.line, describe(path), err)
		}
		v.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n.kind != kindScalar {
			return typeErr(n, path, "a number")
		}
		i, err := strconv.ParseInt(strings.TrimSpace(n.scalar), 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: %s: %q is not an integer", n.line, describe(path), n.scalar)
		}
		if v.OverflowInt(i) {
			return fmt.Errorf("line %d: %s: %d overflows %s", n.line, describe(path), i, v.Type())
		}
		v.SetInt(i)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n.kind != kindScalar {
			return typeErr(n, path, "a number")
		}
		u, err := strconv.ParseUint(strings.TrimSpace(n.scalar), 10, 64)
		if err != nil {
			return fmt.Errorf("line %d: %s: %q is not an unsigned integer", n.line, describe(path), n.scalar)
		}
		if v.OverflowUint(u) {
			return fmt.Errorf("line %d: %s: %d overflows %s", n.line, describe(path), u, v.Type())
		}
		v.SetUint(u)
		return nil
	case reflect.Float32, reflect.Float64:
		if n.kind != kindScalar {
			return typeErr(n, path, "a number")
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(n.scalar), 64)
		if err != nil {
			return fmt.Errorf("line %d: %s: %q is not a number", n.line, describe(path), n.scalar)
		}
		v.SetFloat(f)
		return nil
	case reflect.Interface:
		if n.kind != kindScalar {
			return typeErr(n, path, "a scalar")
		}
		v.Set(reflect.ValueOf(n.scalar))
		return nil
	default:
		return fmt.Errorf("yamlmin: %s: unsupported target type %s", describe(path), v.Type())
	}
}

func assignDuration(n *node, v reflect.Value, path string) error {
	if n.kind != kindScalar {
		return typeErr(n, path, "a duration")
	}
	s := strings.TrimSpace(n.scalar)
	d, err := time.ParseDuration(s)
	if err == nil {
		v.SetInt(int64(d))
		return nil
	}
	// A bare number is accepted as nanoseconds, which is how the previous
	// library treated an integer assigned to a time.Duration field.
	if i, iErr := strconv.ParseInt(s, 10, 64); iErr == nil {
		v.SetInt(i)
		return nil
	}
	return fmt.Errorf("line %d: %s: %q is not a duration (want e.g. 15s, 500ms, 2m)", n.line, describe(path), n.scalar)
}

func assignStruct(n *node, v reflect.Value, path string) error {
	if n.kind != kindMapping {
		return typeErr(n, path, "a set of keys")
	}
	byKey := fieldsByYAMLName(v.Type())
	for _, key := range n.keys {
		f, ok := byKey[key]
		if !ok {
			continue // unknown key: ignored, as the previous library did
		}
		if err := assign(n.fields[key], v.Field(f), join(path, key)); err != nil {
			return err
		}
	}
	return nil
}

func assignSlice(n *node, v reflect.Value, path string) error {
	if n.kind != kindSequence {
		return typeErr(n, path, "a list")
	}
	out := reflect.MakeSlice(v.Type(), len(n.items), len(n.items))
	for i, item := range n.items {
		if err := assign(item, out.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	v.Set(out)
	return nil
}

func assignMap(n *node, v reflect.Value, path string) error {
	if n.kind != kindMapping {
		return typeErr(n, path, "a set of keys")
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("yamlmin: %s: only string-keyed maps are supported", describe(path))
	}
	out := reflect.MakeMapWithSize(v.Type(), len(n.keys))
	for _, key := range n.keys {
		ev := reflect.New(v.Type().Elem()).Elem()
		if err := assign(n.fields[key], ev, join(path, key)); err != nil {
			return err
		}
		out.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), ev)
	}
	v.Set(out)
	return nil
}

// fieldsByYAMLName maps YAML key -> field index for a struct type.
func fieldsByYAMLName(t reflect.Type) map[string]int {
	out := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name := f.Tag.Get("yaml")
		if idx := strings.IndexByte(name, ','); idx >= 0 {
			name = name[:idx]
		}
		if name == "-" {
			continue
		}
		if name == "" {
			// The previous library lower-cased the field name when no tag was
			// present; every field in this codebase is tagged, but matching
			// the old default keeps behaviour identical if one is added.
			name = strings.ToLower(f.Name)
		}
		out[name] = i
	}
	return out
}

// parseBool accepts exactly what YAML 1.2's core schema calls a boolean.
//
// Notably absent: yes/no/on/off. YAML 1.1 treated those as booleans, which is
// the origin of the "Norway problem" — a country code NO silently becoming
// false. The previous library implemented the 1.2 schema and rejected them too,
// so accepting them here would be a behaviour change, not a convenience.
func parseBool(s string) (bool, error) {
	switch strings.TrimSpace(s) {
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	default:
		return false, fmt.Errorf("%q is not a boolean (want true or false)", s)
	}
}

func typeErr(n *node, path, want string) error {
	return fmt.Errorf("line %d: %s: expected %s, found %s", n.line, describe(path), want, n.kind)
}

func (k kind) String() string {
	switch k {
	case kindMapping:
		return "a set of keys"
	case kindSequence:
		return "a list"
	default:
		return "a single value"
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describe(path string) string {
	if path == "" {
		return "document root"
	}
	return path
}
