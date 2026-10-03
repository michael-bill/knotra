package contract

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

type sourcePosition struct{ line, column int }

func sourcePositions(data []byte) map[string]sourcePosition {
	positions := map[string]sourcePosition{}
	doc, err := parseYAML(data)
	if err != nil {
		return positions
	}
	var visit func(*yaml.Node, string)
	visit = func(n *yaml.Node, path string) {
		positions[path] = sourcePosition{n.Line, n.Column}

		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i < len(n.Content); i += 2 {
				key := strings.ReplaceAll(strings.ReplaceAll(n.Content[i].Value, "~", "~0"), "/", "~1")
				visit(n.Content[i+1], path+"/"+key)
			}
		case yaml.SequenceNode:
			for i, child := range n.Content {
				visit(child, path+"/"+strconv.Itoa(i))
			}
		}
	}
	visit(doc.Content[0], "")
	return positions
}

func locateDiagnostic(d Diagnostic, data []byte) Diagnostic {
	return locateWithPositions(d, sourcePositions(data))
}

func locateWithPositions(d Diagnostic, positions map[string]sourcePosition) Diagnostic {
	candidate := d.Path

	for {
		if p, ok := positions[candidate]; ok {
			d.Line = p.line
			d.Column = p.column
			return d
		}
		i := strings.LastIndex(candidate, "/")
		if i < 0 {
			return d
		}
		candidate = candidate[:i]
	}
}

func diagnosticError(code, phase, file, path string, err error) Diagnostic {
	d := Diagnostic{Severity: "error", Code: code, Phase: phase, File: file, Path: path, Message: err.Error()}
	var source *sourceError
	if errors.As(err, &source) {
		d.Line = source.line
		d.Column = source.column
		return d
	}
	var validation *jsonschema.ValidationError
	if errors.As(err, &validation) {
		deepest := validation
		var visit func(*jsonschema.ValidationError)
		visit = func(v *jsonschema.ValidationError) {
			if len(v.InstanceLocation) > len(deepest.InstanceLocation) {
				deepest = v
			}

			for _, cause := range v.Causes {
				visit(cause)
			}
		}
		visit(validation)
		segments := []string{}

		for _, s := range deepest.InstanceLocation {
			segments = append(segments, strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1"))
		}

		if len(segments) > 0 {
			d.Path = "/" + strings.Join(segments, "/")
		}
	}
	return d
}

func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}
