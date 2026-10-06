package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/michael-bill/knotra/schemas"
)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}

type forbiddenLoader struct{}

func (forbiddenLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema loading forbidden: %s", url)
}

type ecmaRegexp struct{ *regexp2.Regexp }

func (r ecmaRegexp) MatchString(s string) bool {
	ok, err := r.Regexp.MatchString(s)
	return err == nil && ok
}

func structuralRegexp(pattern string) (jsonschema.Regexp, error) {
	r, e := regexp2.Compile(pattern, regexp2.ECMAScript)
	if e != nil {
		return nil, e
	}
	r.MatchTimeout = time.Second
	return ecmaRegexp{r}, nil
}

var structuralOnce sync.Once

var structuralSchema *jsonschema.Schema

var structuralError error

// ValidateStructure checks the closed, versioned YAML object grammar.
func ValidateStructure(v any) error {
	structuralOnce.Do(func() {
		structuralError = initializeStructuralSchema()
	})
	if structuralError != nil {
		return structuralError
	}
	return structuralSchema.Validate(v)
}

// JSONValue constructs a JSON value without permitting forged artifact handles.
func JSONValue(v any) (Value, error) {
	if err := nativeUnicode(reflect.ValueOf(v), 0); err != nil {
		return Value{}, err
	}
	b, err := marshalJSON(v)
	if err != nil {
		return Value{}, err
	}
	parsed, err := DecodeJSON(b)
	if err != nil {
		return Value{}, err
	}
	b, err = marshalJSON(parsed)
	return Value{JSON: b}, err
}

func nativeUnicode(v reflect.Value, depth int) error {
	if !v.IsValid() {
		return nil
	}
	if depth > 4*(MaxDepth+1) {
		return fmt.Errorf("native value exceeds recursion limit")
	}

	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid Unicode string")
		}
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return nativeUnicode(v.Elem(), depth+1)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if err := nativeUnicode(key, depth+1); err != nil {
				return err
			}
			if err := nativeUnicode(v.MapIndex(key), depth+1); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := nativeUnicode(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanInterface() {
				if err := nativeUnicode(v.Field(i), depth+1); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

var dataSchemas sync.Map // immutable compiled schemas, shared across runtime calls

func compileDataSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	key := string(raw)
	if value, ok := dataSchemas.Load(key); ok {
		schema, ok := value.(*jsonschema.Schema)
		if !ok {
			return nil, fmt.Errorf("invalid cached JSON schema")
		}
		return schema, nil
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("unresolved or absent JSON schema")
	}
	if len(raw) > 256<<10 {
		return nil, fmt.Errorf("schema exceeds 256 KiB")
	}
	value, err := DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if err := validateDataProfile(value, value, map[string]bool{}, 0); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(forbiddenLoader{})
	c.UseRegexpEngine(func(s string) (jsonschema.Regexp, error) { return regexp.Compile(s) })
	if err := c.AddResource("urn:knotra:data", value); err != nil {
		return nil, err
	}
	schema, err := c.Compile("urn:knotra:data")
	if err != nil {
		return nil, err
	}
	dataSchemas.Store(key, schema)
	return schema, nil
}

func initializeStructuralSchema() error {
	s, err := DecodeJSON(schemas.V1)
	if err != nil {
		structuralError = err
		return err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(forbiddenLoader{})
	c.UseRegexpEngine(structuralRegexp)
	if err = c.AddResource("urn:knotra:schema:v1", s); err == nil {
		structuralSchema, err = c.Compile("urn:knotra:schema:v1")
	}
	structuralError = err
	return err
}

var schemaKeys = func() map[string]bool {
	m := map[string]bool{}

	for _, s := range strings.Fields("$schema $ref $defs $comment title description type const enum default examples properties patternProperties additionalProperties propertyNames required minProperties maxProperties dependentRequired dependentSchemas items prefixItems contains minContains maxContains minItems maxItems uniqueItems minLength maxLength pattern minimum maximum exclusiveMinimum exclusiveMaximum multipleOf allOf anyOf oneOf not if then else") {
		m[s] = true
	}

	return m
}()

func validateDataProfile(value, root any, stack map[string]bool, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("schema reference depth exceeds 64")
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be boolean or object")
	}

	for _, k := range sortedKeys(m) {
		v := m[k]
		if !schemaKeys[k] {
			return fmt.Errorf("unsupported DataSchema keyword %q", k)
		}

		switch k {
		case "$schema":
			if v != "https://json-schema.org/draft/2020-12/schema" {
				return fmt.Errorf("unsupported schema draft")
			}
		case "$ref":
			s, ok := v.(string)
			if !ok || !strings.HasPrefix(s, "#") {
				return fmt.Errorf("only local schema references permitted")
			}
			if stack[s] {
				return fmt.Errorf("cyclic schema reference %s", s)
			}
			fragment, err := url.PathUnescape(strings.TrimPrefix(s, "#"))
			if err != nil {
				return err
			}
			target, present, err := pointer(root, fragment)
			if err != nil || !present {
				return fmt.Errorf("unresolved schema reference %s", s)
			}
			stack[s] = true
			err = validateDataProfile(target, root, stack, depth+1)
			delete(stack, s)
			if err != nil {
				return err
			}
		case "pattern":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("pattern must be string")
			}
			if err := portableRegexp(s); err != nil {
				return err
			}
		case "$defs", "properties", "patternProperties", "dependentSchemas":
			obj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s must be object", k)
			}
			for _, name := range sortedKeys(obj) {
				if k == "patternProperties" {
					if err := portableRegexp(name); err != nil {
						return err
					}
				}
				if err := validateDataProfile(obj[name], root, stack, depth+1); err != nil {
					return err
				}
			}
		case "additionalProperties", "propertyNames", "items", "contains", "not", "if", "then", "else":
			if err := validateDataProfile(v, root, stack, depth+1); err != nil {
				return err
			}
		case "prefixItems", "allOf", "anyOf", "oneOf":
			a, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s must be array", k)
			}
			for _, item := range a {
				if err := validateDataProfile(item, root, stack, depth+1); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func portableRegexp(s string) error {
	if strings.Contains(strings.ReplaceAll(s, "(?:", "("), "(?") {
		return fmt.Errorf("special regexp groups are forbidden")
	}

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			if i == len(s) {
				break
			}
			if !strings.ContainsRune(`dDsSwWtrn\\.^$|?*+()[]{}-/`, rune(s[i])) {
				return fmt.Errorf("unsupported regexp escape \\%c", s[i])
			}
		}
	}

	_, err := regexp.Compile(s)
	return err
}

// ValidateValue validates one typed value against its resolved port contract.
func ValidateValue(p Port, v Value) error {
	if p.Artifact != nil {
		if len(v.JSON) != 0 || v.Collection != p.Artifact.Collection || (!v.Collection && len(v.Artifacts) != 1) {
			return fmt.Errorf("artifact shape differs from port")
		}

		for _, a := range v.Artifacts {
			if a.ID == "" || a.Size < 0 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(a.SHA256) {
				return fmt.Errorf("invalid artifact descriptor")
			}
			if !contains(p.Artifact.MediaTypes, a.MediaType) {
				return fmt.Errorf("artifact media type %q not allowed", a.MediaType)
			}
		}

		return nil
	}
	if len(v.JSON) == 0 || len(v.Artifacts) != 0 || v.Collection {
		return fmt.Errorf("expected JSON value")
	}
	data, err := DecodeJSON(v.JSON)
	if err != nil {
		return err
	}
	schema, err := compileDataSchema(p.Schema)
	if err != nil {
		return err
	}
	return schema.Validate(data)
}

// ValidatePorts checks unknown/missing names, then inserts only InputPort
// defaults. JSON Schema annotations never mutate user values.
func ValidatePorts(ports map[string]Port, values Values, applyDefaults bool) (Values, error) {
	out := Values{}

	for _, name := range sortedKeys(values) {
		if _, ok := ports[name]; !ok {
			return nil, fmt.Errorf("unknown port %q", name)
		}
	}

	serialized := map[string]any{}

	for _, name := range sortedKeys(ports) {
		p := ports[name]
		v, ok := values[name]
		if !ok && applyDefaults && len(p.Default) > 0 {
			v = Value{JSON: bytes.Clone(p.Default)}
			ok = true
		}
		if !ok {
			if p.IsRequired() {
				return nil, fmt.Errorf("missing required port %q", name)
			}
			continue
		}
		if err := ValidateValue(p, v); err != nil {
			return nil, fmt.Errorf("port %s: %w", name, err)
		}
		if len(v.JSON) > 0 {
			data, err := DecodeJSON(v.JSON)
			if err != nil {
				return nil, err
			}
			normalized, err := JSONValue(data)
			if err != nil {
				return nil, err
			}
			v.JSON = normalized.JSON
		}
		switch {
		case len(v.JSON) > 0:
			serialized[name] = v.JSON
		case v.Collection:
			items := []any{}

			for _, a := range v.Artifacts {
				items = append(items, artifactDescriptor(a))
			}

			serialized[name] = items
		default:
			serialized[name] = artifactDescriptor(v.Artifacts[0])

		}
		out[name] = v
	}

	encoded, err := json.Marshal(serialized)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxValueBytes {
		return nil, fmt.Errorf("port values exceed 16 MiB")
	}
	return out, nil
}

// PortObjectSchema is the strict JSON output-object schema presented to models.
// Artifacts are collected independently and excluded from this object.
func PortObjectSchema(ports map[string]Port) json.RawMessage {
	properties := map[string]any{}
	required := []string{}

	for _, name := range sortedKeys(ports) {
		p := ports[name]
		if p.Artifact != nil {
			continue
		}
		s, err := DecodeJSON(p.Schema)
		if err != nil {
			s = false
		}
		properties[name] = rewriteRefs(s, "/properties/"+strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1"))
		if p.IsRequired() {
			required = append(required, name)
		}
	}

	raw, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	return raw
}

// ContextEnvelope keeps JSON inputs and artifact descriptors in separate maps.
func ContextEnvelope(values Values) map[string]any {
	jsonValues := map[string]any{}
	artifacts := map[string]any{}

	for _, k := range sortedKeys(values) {
		v := values[k]
		switch {
		case len(v.JSON) > 0:
			x, err := DecodeJSON(v.JSON)
			if err == nil {
				jsonValues[k] = x
			}
		case v.Collection:
			descriptors := make([]any, 0, len(v.Artifacts))

			for _, a := range v.Artifacts {
				descriptors = append(descriptors, artifactDescriptor(a))
			}

			artifacts[k] = descriptors
		case len(v.Artifacts) == 1:
			artifacts[k] = artifactDescriptor(v.Artifacts[0])

		}
	}

	return map[string]any{"values": jsonValues, "artifacts": artifacts}
}

func artifactDescriptor(a Artifact) map[string]any {
	m := map[string]any{"id": a.ID, "mediaType": a.MediaType, "size": a.Size, "sha256": a.SHA256}
	if a.Path != "" {
		m["path"] = a.Path
	}
	return m
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}
