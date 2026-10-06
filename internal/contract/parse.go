package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	MaxDocumentBytes = 8 << 20
	MaxValueBytes    = 16 << 20
	MaxDepth         = 64
)

var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

var yamlNumberLike = regexp.MustCompile(`(?i)^[+-]?(?:[0-9][0-9_]*(?:\.[0-9_]*)?(?:e[+-]?[0-9_]+)?|\.[0-9_]+|\.inf|\.nan|0[xob][0-9a-f_]+)$`)

var durationPattern = regexp.MustCompile(`^[1-9][0-9]*(ms|s|m|h)$`)

// Duration implements the bounded single-unit duration grammar of Knotra v1.
func Duration(s string) (time.Duration, error) {
	if !durationPattern.MatchString(s) {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d > 365*24*time.Hour {
		return 0, fmt.Errorf("duration exceeds 365 days: %q", s)
	}
	return d, nil
}

func number(s string) (any, error) {
	if !jsonNumber.MatchString(s) {
		return nil, fmt.Errorf("invalid JSON number %q", s)
	}
	if !strings.ContainsAny(s, ".eE") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer outside int64: %q", s)
		}
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, fmt.Errorf("number outside finite float64: %q", s)
	}
	if f == 0 {
		significand := strings.Split(strings.ToLower(s), "e")[0]
		if strings.ContainsAny(significand, "123456789") {
			return nil, fmt.Errorf("floating point underflow: %q", s)
		}
	}
	return f, nil
}

// ParseDocument decodes the deliberately restricted YAML 1.2 / JSON source format.
// The syntax tree is checked before maps are constructed, preserving duplicates.
func ParseDocument(data []byte) (any, error) {
	doc, err := parseYAML(data)
	if err != nil {
		return nil, err
	}
	return yamlValue(doc.Content[0], 0)
}

type sourceError struct {
	line, column int
	message      string
}

func (e *sourceError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.line, e.column, e.message)
}

func parseYAML(data []byte) (*yaml.Node, error) {
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("document exceeds 8 MiB")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("source is not valid UTF-8")
	}
	// yaml.v3 uses 1.2 scalar rules, but only accepts the obsolete 1.1 directive.
	// Strip the sole directive allowed by our grammar without changing line numbers.
	lines := strings.Split(string(data), "\n")
	directiveSeen, documentStarted := false, false

	for i, line := range lines {
		if strings.HasPrefix(line, "%") {
			if strings.TrimSpace(strings.SplitN(line, "#", 2)[0]) != "%YAML 1.2" || directiveSeen || documentStarted {
				return nil, &sourceError{i + 1, 1, "unsupported, duplicate or misplaced YAML directive"}
			}
			directiveSeen = true
			lines[i] = "# YAML 1.2"
		} else if !documentStarted && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			if directiveSeen && strings.TrimSpace(line) != "---" {
				return nil, &sourceError{i + 1, 1, "YAML directive requires explicit document start"}
			}
			documentStarted = true
		}
	}

	dec := yaml.NewDecoder(strings.NewReader(strings.Join(lines, "\n")))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("expected one nonempty document")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("multiple YAML documents are forbidden")
	}
	return &doc, nil
}

func yamlValue(n *yaml.Node, depth int) (any, error) {
	fail := func(message string) (any, error) {
		return nil, &sourceError{n.Line, n.Column, message}
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return fail("anchors and aliases are forbidden")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return fail("explicit YAML tags are forbidden")
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		depth++
		if depth > MaxDepth {
			return fail("container depth exceeds 64")
		}
	}

	switch n.Kind {
	case yaml.MappingNode:
		result := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			key, err := yamlValue(keyNode, depth)
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return fail("mapping keys must be strings")
			}
			if k == "<<" {
				return fail("merge keys are forbidden")
			}
			if _, ok := result[k]; ok {
				return fail("duplicate key " + strconv.Quote(k))
			}
			v, err := yamlValue(n.Content[i+1], depth)
			if err != nil {
				return nil, err
			}
			result[k] = v
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := yamlValue(child, depth)
			if err != nil {
				return nil, err
			}
			result = append(result, v)
		}
		return result, nil
	case yaml.ScalarNode:
		// yaml.v3 resolves overflowing numeric tokens as strings. Numeric JSON
		// lexemes in plain style remain numbers in the Knotra grammar.
		if n.Style == 0 && yamlNumberLike.MatchString(n.Value) {
			v, err := number(n.Value)
			if err != nil {
				return fail(err.Error())
			}
			return v, nil
		}
		switch n.Tag {
		case "!!str", "!!timestamp":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			return strings.EqualFold(n.Value, "true"), nil
		case "!!int", "!!float":
			return number(n.Value)
		default:
			return fail("unsupported scalar type " + n.Tag)
		}
	default:
		return fail("unsupported YAML node")
	}
}

// DecodeJSON rejects duplicate keys, invalid Unicode, oversized values, nonfinite
// numbers and precision-losing integer conversion at every runtime boundary.
func DecodeJSON(data []byte) (any, error) {
	return DecodeJSONLimit(data, MaxValueBytes)
}

// DecodeJSONLimit applies the same lossless JSON grammar to transport envelopes
// with a distinct byte quota. Port values still use the 16 MiB DecodeJSON limit.
func DecodeJSONLimit(data []byte, maximumBytes int) (any, error) {
	if maximumBytes < 0 || len(data) > maximumBytes {
		return nil, fmt.Errorf("JSON exceeds %d byte limit", maximumBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	if err := checkSurrogates(data); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON content")
	}
	return v, nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}

	switch x := t.(type) {
	case json.Number:
		return number(string(x))
	case json.Delim:
		depth++
		if depth > MaxDepth {
			return nil, fmt.Errorf("container depth exceeds 64")
		}
		if x == '{' {
			m := map[string]any{}

			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				s, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("invalid object key")
				}
				if _, ok := m[s]; ok {
					return nil, fmt.Errorf("duplicate key %q", s)
				}
				v, err := jsonValue(d, depth)
				if err != nil {
					return nil, err
				}
				m[s] = v
			}

			_, err = d.Token()
			return m, err
		}
		if x == '[' {
			a := []any{}

			for d.More() {
				v, err := jsonValue(d, depth)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}

			_, err = d.Token()
			return a, err
		}
		return nil, fmt.Errorf("unexpected delimiter")
	default:
		return t, nil
	}
}

func checkSurrogates(b []byte) error {
	inside := false

	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			break
		}
		n, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if err != nil {
			continue
		}
		i += 4
		if n >= 0xDC00 && n <= 0xDFFF {
			return fmt.Errorf("unpaired low surrogate")
		}
		if n >= 0xD800 && n <= 0xDBFF {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return fmt.Errorf("unpaired high surrogate")
			}
			low, err := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return fmt.Errorf("unpaired high surrogate")
			}
			i += 6
		}
	}

	return nil
}

// marshalJSON preserves the int/double distinction required by CEL even when a
// floating point value is mathematically integral.
func marshalJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, fmt.Errorf("nonfinite number")
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return []byte(s), nil
	case map[string]any:
		var b bytes.Buffer
		_ = b.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				_ = b.WriteByte(',')
			}
			key, _ := json.Marshal(k)
			_, _ = b.Write(key)
			_ = b.WriteByte(':')
			value, err := marshalJSON(x[k])
			if err != nil {
				return nil, err
			}
			_, _ = b.Write(value)
		}
		_ = b.WriteByte('}')
		return b.Bytes(), nil
	case []any:
		var b bytes.Buffer
		_ = b.WriteByte('[')
		for i, v := range x {
			if i > 0 {
				_ = b.WriteByte(',')
			}
			value, err := marshalJSON(v)
			if err != nil {
				return nil, err
			}
			_, _ = b.Write(value)
		}
		_ = b.WriteByte(']')
		return b.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}
