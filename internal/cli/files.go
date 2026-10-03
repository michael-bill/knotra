package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/contract"
)

func (s *commandState) readFile(name string, max int64) ([]byte, error) {
	var reader io.Reader = s.options.In
	var file *os.File
	var err error
	if name != "-" {
		file, err = os.Open(name)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader = file
	}
	b, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	return b, nil
}
func (s *commandState) object(file string, assignments []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if file != "" {
		b, err := s.readFile(file, 16<<20)
		if err != nil {
			return nil, err
		}
		value, err := contract.DecodeJSON(b)
		if err != nil {
			return nil, err
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must contain one JSON object", file)
		}
		for key, value := range object {
			data, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			out[key] = data
		}
	}
	for _, assignment := range assignments {
		name, text, ok := strings.Cut(assignment, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("expected NAME=JSON, got %q", assignment)
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("input %q supplied more than once", name)
		}
		value, err := contract.DecodeJSON([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}
func (s *commandState) artifactBindings(file string, assignments []string) (map[string]json.RawMessage, error) {
	object, err := s.object(file, nil)
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("expected NAME=ARTIFACT_ID or NAME=[IDS]")
		}
		if _, exists := object[name]; exists {
			return nil, fmt.Errorf("artifact input %q supplied twice", name)
		}
		var raw []byte
		if strings.HasPrefix(value, "[") {
			raw = []byte(value)
		} else {
			raw, _ = json.Marshal(value)
		}
		object[name] = raw
	}
	for name, raw := range object {
		var single string
		var collection []string
		if json.Unmarshal(raw, &single) == nil && single != "" {
			continue
		}
		if json.Unmarshal(raw, &collection) != nil || string(raw) == "null" {
			return nil, fmt.Errorf("artifact input %q must be an ID or list of IDs", name)
		}
		for _, id := range collection {
			if id == "" {
				return nil, fmt.Errorf("artifact ID must not be empty")
			}
		}
	}
	return object, nil
}
func loadPackage(file, root string) (contract.Package, error) {
	if root == "" {
		return contract.LoadPackage(file)
	}
	absolute, err := filepath.Abs(file)
	if err != nil {
		return contract.Package{}, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return contract.Package{}, err
	}
	relative, err := filepath.Rel(absoluteRoot, absolute)
	if err != nil {
		return contract.Package{}, err
	}
	return contract.LoadPackageRoot(absoluteRoot, filepath.ToSlash(relative))
}
func operationKey(value string) string {
	if value != "" {
		return value
	}
	return uuid.NewString()
}
func definitionKey(value string) string {
	sum := sha256.Sum256([]byte("definition/" + value))
	return hex.EncodeToString(sum[:])
}
func atomicFile(name string, data []byte, replace bool) error {
	dir := filepath.Dir(name)
	file, err := os.CreateTemp(dir, ".knotra-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(file.Name(), name)
	} else {
		err = os.Link(file.Name(), name)
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
