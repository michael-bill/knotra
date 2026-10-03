package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"golang.org/x/text/unicode/norm"
)

func validPath(name string) error {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || !norm.NFC.IsNormalString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid package path %q", name)
	}
	if len(name) >= 2 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':' {
		return fmt.Errorf("drive-prefixed path forbidden")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path segment")
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("control character in path")
		}
	}
	return nil
}

func foldPath(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return unicode.ToLower(r)
		}
		return r
	}, s)
}

// LoadPackage loads only explicitly admitted files relative to the entrypoint's
// directory. It never copies unrelated workspace files or follows symlinks.
func LoadPackage(entrypoint string) (Package, error) {
	absolute, err := filepath.Abs(entrypoint)
	if err != nil {
		return Package{}, err
	}
	return LoadPackageRoot(filepath.Dir(absolute), filepath.Base(absolute))
}

// LoadPackageRoot permits an entrypoint below an explicitly selected package
// root. All references remain relative to root, including those in imports.
func LoadPackageRoot(root, entrypoint string) (Package, error) {
	if err := validPath(entrypoint); err != nil {
		return Package{}, err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return Package{}, err
	}
	defer fs.Close()
	files := map[string][]byte{}
	total := 0
	read := func(name string) ([]byte, error) {
		if err := validPath(name); err != nil {
			return nil, err
		}
		if b, ok := files[name]; ok {
			return b, nil
		}
		segments := strings.Split(name, "/")
		for i := range segments {
			info, err := fs.Lstat(strings.Join(segments[:i+1], "/"))
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("symlink forbidden: %s", name)
			}
		}
		f, err := fs.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("not a regular file: %s", name)
		}
		stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
		if stat.IsValid() && stat.Kind() == reflect.Struct {
			nlink := stat.FieldByName("Nlink")
			if nlink.IsValid() && nlink.CanUint() && nlink.Uint() > 1 {
				return nil, fmt.Errorf("hard links forbidden: %s", name)
			}
		}
		if info.Size() > 64<<20 {
			return nil, fmt.Errorf("package exceeds 64 MiB")
		}
		b, err := io.ReadAll(io.LimitReader(f, (64<<20)+1))
		if err != nil {
			return nil, err
		}
		total += len(b)
		if total > 64<<20 || len(files) >= 512 {
			return nil, fmt.Errorf("package size/file limit exceeded")
		}
		files[name] = b
		return b, nil
	}
	admitted := map[string]bool{entrypoint: true}
	seen := map[string]bool{}
	var visit func(string, int) error
	visit = func(name string, depth int) error {
		if depth > 32 {
			return fmt.Errorf("import depth exceeds 32")
		}
		if seen[name] {
			return nil
		}
		seen[name] = true
		b, err := read(name)
		if err != nil {
			return err
		}
		p, diags := parsePipeline(b, name)
		if len(diags) > 0 {
			return fmt.Errorf("%s: %s", name, diags[0].Message)
		}
		for _, file := range p.Spec.Files {
			admitted[file] = true
			if _, err := read(file); err != nil {
				return err
			}
		}
		for _, edge := range importEdges(p.Spec.Graph, 0) {
			child := edge.file
			if !admitted[child] {
				return fmt.Errorf("import %s not declared in spec.files", child)
			}
			if err := visit(child, depth+edge.depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(entrypoint, 0); err != nil {
		return Package{}, err
	}
	p := Package{Entrypoint: entrypoint, Source: string(files[entrypoint])}
	for _, name := range sortedKeys(files) {
		p.Files = append(p.Files, File{Path: name, Content: files[name]})
	}
	return p, nil
}

type importEdge struct {
	file  string
	depth int
}

func importEdges(g Graph, depth int) []importEdge {
	out := []importEdge{}
	for _, name := range sortedKeys(g.Nodes) {
		n := g.Nodes[name]
		if n.Pipeline != nil {
			out = append(out, importEdge{n.Pipeline.File, depth})
		}
		if n.Foreach != nil {
			out = append(out, importEdges(n.Foreach.Body, depth+1)...)
		}
		if n.Loop != nil {
			out = append(out, importEdges(n.Loop.Body, depth+1)...)
		}
	}
	return out
}

func packageFiles(p Package) (map[string][]byte, string, error) {
	if err := validPath(p.Entrypoint); err != nil {
		return nil, "", err
	}
	files := map[string][]byte{}
	names := map[string]string{}
	total := 0
	type manifestFile struct {
		Path   string `json:"path"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	}
	manifest := struct {
		Entrypoint string         `json:"entrypoint"`
		Files      []manifestFile `json:"files"`
	}{Entrypoint: p.Entrypoint}
	if len(p.Files) > 512 {
		return nil, "", fmt.Errorf("package exceeds 512 files")
	}
	for _, f := range p.Files {
		if err := validPath(f.Path); err != nil {
			return nil, "", err
		}
		fold := foldPath(f.Path)
		if previous, ok := names[fold]; ok {
			return nil, "", fmt.Errorf("duplicate/colliding package paths %q and %q", previous, f.Path)
		}
		names[fold] = f.Path
		files[f.Path] = f.Content
		total += len(f.Content)
	}
	if total > 64<<20 {
		return nil, "", fmt.Errorf("package exceeds 64 MiB")
	}
	source, ok := files[p.Entrypoint]
	if !ok {
		return nil, "", fmt.Errorf("entrypoint absent from package")
	}
	if p.Source != "" && p.Source != string(source) {
		return nil, "", fmt.Errorf("source differs from entrypoint bytes")
	}
	for _, name := range sortedKeys(files) {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			if _, exists := names[foldPath(strings.Join(parts[:i], "/"))]; exists {
				return nil, "", fmt.Errorf("package file/directory collision")
			}
		}
		hash := sha256.Sum256(files[name])
		manifest.Files = append(manifest.Files, manifestFile{name, len(files[name]), hex.EncodeToString(hash[:])})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", err
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return files, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func diagnostic(code, phase, file, path string, err error) Diagnostic {
	return diagnosticError(code, phase, file, path, err)
}
func parsePipeline(data []byte, file string) (Pipeline, []Diagnostic) {
	var p Pipeline
	value, err := ParseDocument(data)
	if err != nil {
		return p, []Diagnostic{diagnostic("PARSE_INVALID", "parse", file, "", err)}
	}
	if err = ValidateStructure(value); err != nil {
		return p, []Diagnostic{locateDiagnostic(diagnostic("STRUCTURE_INVALID", "structural", file, "", err), data)}
	}
	raw, err := marshalJSON(value)
	if err != nil {
		return p, []Diagnostic{diagnostic("PARSE_INVALID", "parse", file, "", err)}
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, []Diagnostic{diagnostic("STRUCTURE_INVALID", "structural", file, "", err)}
	}
	if p.Kind != "Pipeline" {
		return p, []Diagnostic{diagnostic("DOCUMENT_KIND", "package", file, "/kind", fmt.Errorf("expected Pipeline"))}
	}
	return p, nil
}

// ParseProfile parses and validates the trusted configuration, without resolving
// environment variables, loading images or contacting external services.
func ParseProfile(data []byte) (Profile, []Diagnostic) {
	var p Profile
	value, err := ParseDocument(data)
	if err != nil {
		return p, []Diagnostic{diagnostic("PARSE_INVALID", "parse", "", "", err)}
	}
	if err = ValidateStructure(value); err != nil {
		return p, []Diagnostic{diagnostic("STRUCTURE_INVALID", "structural", "", "", err)}
	}
	raw, err := marshalJSON(value)
	if err != nil {
		return p, []Diagnostic{diagnostic("PARSE_INVALID", "parse", "", "", err)}
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, []Diagnostic{diagnostic("STRUCTURE_INVALID", "structural", "", "", err)}
	}
	if p.Kind != "EngineProfile" {
		return p, []Diagnostic{diagnostic("DOCUMENT_KIND", "semantic", "", "/kind", fmt.Errorf("expected EngineProfile"))}
	}
	diags := validateProfile(p)
	for i := range diags {
		diags[i] = locateDiagnostic(diags[i], data)
	}
	sortDiagnostics(diags)
	return p, diags
}
