// Package bootstrap prepares the single-machine development setup. It never
// deletes state or runs pipeline commands on the host.
package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	assets "github.com/michael-bill/knotra"
	"github.com/michael-bill/knotra/examples/starter"
	"github.com/michael-bill/knotra/internal/contract"
)

type Config struct {
	Dir, Provider, Model, HelperPath string
	PostgresPort                     int
}

type Setup struct {
	Config Config
	Out    io.Writer
	Getenv func(string) string
	// Command is an injectable boundary for trusted infrastructure commands.
	Command        func(context.Context, string, []string, []string) ([]byte, error)
	compose        string
	arch           string
	helper         []byte
	modelInstalled bool
}

type settings struct {
	Config      Config `json:"config"`
	Password    string `json:"password"`
	OperationID string `json:"operationId"`
}

func (s *Setup) run(ctx context.Context, executable string, args []string, env []string) ([]byte, error) {
	if s.Command != nil {
		return s.Command(ctx, executable, args, env)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append(os.Environ(), env...)
	var output commandOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	data := output.Bytes()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %s", executable, strings.Join(args, " "), strings.TrimSpace(string(data)))
	}
	return data, nil
}

// Infrastructure commands can print large pull logs. Keep only the final 64 KiB
// for diagnostics; no pipeline command uses this host execution boundary.
type commandOutput struct{ bytes.Buffer }

func (b *commandOutput) Write(data []byte) (int, error) {
	size := len(data)
	const limit = 64 << 10
	if len(data) >= limit {
		b.Reset()
		data = data[len(data)-limit:]
	}
	if b.Len()+len(data) > limit {
		b.Next(b.Len() + len(data) - limit)
	}
	_, err := b.Buffer.Write(data)
	return size, err
}

func (s *Setup) Validate() error {
	if s.Out == nil {
		s.Out = io.Discard
	}
	if s.Getenv == nil {
		s.Getenv = os.Getenv
	}
	if s.Config.Provider != "ollama" && s.Config.Provider != "openai" && s.Config.Provider != "anthropic" {
		return fmt.Errorf("--provider must be ollama, openai or anthropic")
	}
	if s.Config.Model == "" {
		return fmt.Errorf("--model is required for cloud providers")
	}
	if s.Config.PostgresPort < 1 || s.Config.PostgresPort > 65535 {
		return fmt.Errorf("PostgreSQL port must be from 1 to 65535")
	}
	if s.Config.Dir == "" {
		return fmt.Errorf("quickstart directory is required")
	}
	dir, err := filepath.Abs(s.Config.Dir)
	if err != nil {
		return err
	}
	s.Config.Dir = dir
	return nil
}

// Doctor performs read-only checks. Downloads and service startup are explicit
// later steps, so a missing credential fails before creating infrastructure.
func (s *Setup) Doctor(ctx context.Context) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := s.run(ctx, "docker", []string{"info", "--format", "{{.OSType}} {{.Architecture}}"}, nil)
	if err != nil {
		return fmt.Errorf("start Docker first (on macOS: colima start): %w", err)
	}
	info := strings.Fields(string(data))
	if len(info) != 2 || info[0] != "linux" {
		return fmt.Errorf("docker must run Linux containers; on Windows switch Docker Desktop to Linux containers")
	}
	s.arch = info[1]
	switch s.arch {
	case "aarch64":
		s.arch = "arm64"
	case "x86_64":
		s.arch = "amd64"
	}
	if s.arch != "arm64" && s.arch != "amd64" {
		return fmt.Errorf("unsupported Docker architecture %q", s.arch)
	}
	if _, err = s.run(ctx, "docker", []string{"compose", "version"}, nil); err != nil {
		if _, err = s.run(ctx, "docker-compose", []string{"version"}, nil); err != nil {
			return fmt.Errorf("install Docker Compose (docker compose or docker-compose)")
		}
		s.compose = "docker-compose"
	} else {
		s.compose = "docker"
	}
	if s.Config.HelperPath == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		s.Config.HelperPath = filepath.Join(filepath.Dir(executable), "sandbox-helper-linux-"+s.arch)
		if _, err = os.Stat(s.Config.HelperPath); err != nil {
			s.Config.HelperPath = filepath.Join(".knotra", "bin", "sandbox-helper")
		}
	}
	helper, err := os.ReadFile(s.Config.HelperPath)
	if err != nil {
		return fmt.Errorf("sandbox helper missing: use the release bundle or run make helper; override with --sandbox-helper")
	}
	if len(helper) < 20 || string(helper[:4]) != "\x7fELF" {
		return fmt.Errorf("sandbox helper must be a Linux ELF executable")
	}
	machine := uint16(helper[18]) | uint16(helper[19])<<8
	if (s.arch == "arm64" && machine != 183) || (s.arch == "amd64" && machine != 62) {
		return fmt.Errorf("sandbox helper architecture does not match Docker (%s)", s.arch)
	}
	s.helper = helper
	s.Config.HelperPath, err = filepath.Abs(s.Config.HelperPath)
	if err != nil {
		return err
	}
	if s.Config.Provider == "ollama" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:11434/api/tags", nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			return fmt.Errorf("start Ollama first (ollama serve)")
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("ollama returned HTTP %d", res.StatusCode)
		}
		var tags struct {
			Models []struct{ Name, Model string }
		}
		if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&tags) != nil {
			return fmt.Errorf("ollama returned an invalid model catalog")
		}
		found := false
		for _, model := range tags.Models {
			if model.Name == s.Config.Model || model.Model == s.Config.Model {
				found = true
			}
		}
		s.modelInstalled = found
		if !found {
			_, _ = fmt.Fprintf(s.Out, "Model %s will be downloaded on startup.\n", s.Config.Model)
			if _, err := exec.LookPath("ollama"); err != nil {
				return fmt.Errorf("install the Ollama CLI to download %s, or select an installed --model", s.Config.Model)
			}
		}
	} else {
		key := strings.ToUpper(s.Config.Provider) + "_API_KEY"
		if s.Getenv(key) == "" {
			return fmt.Errorf("set %s in the engine environment before starting", key)
		}
	}
	_, _ = fmt.Fprintf(s.Out, "Ready: Docker %s, Compose, sandbox helper and %s.\n", s.arch, s.Config.Provider)
	return nil
}

func (s *Setup) profile() contract.Profile {
	c := contract.ModelConnection{Provider: s.Config.Provider, Model: s.Config.Model}
	secrets := map[string]contract.SecretSource{}
	if c.Provider == "ollama" {
		c.Parameters = map[string]any{"think": false, "temperature": 0, "num_predict": 4096}
	} else {
		secrets["model_key"] = contract.SecretSource{Env: strings.ToUpper(c.Provider) + "_API_KEY"}
		c.Auth = map[string]contract.Credential{"key": {SecretRef: "model_key"}}
		if c.Provider == "openai" {
			c.Parameters = map[string]any{"max_output_tokens": 4096}
		} else {
			c.Parameters = map[string]any{"max_tokens": 4096}
		}
	}
	return contract.Profile{APIVersion: "knotra/v1", Kind: "EngineProfile", Metadata: contract.Metadata{Name: "local"}, Spec: contract.ProfileSpec{
		Models: map[string]contract.ModelConnection{"model_main": c}, Secrets: secrets,
		Sandboxes: map[string]contract.SandboxProfile{
			"python_box": {Image: "python:3.13-alpine", Resources: contract.Resources{CPU: 1, MemoryMiB: 512, DiskMiB: 128, Pids: 64}, Network: contract.Network{Mode: "none"}, AllowedTools: []string{"files.read", "files.write", "process.exec"}},
			"node_box":   {Image: "node:22-alpine", Resources: contract.Resources{CPU: 1, MemoryMiB: 512, DiskMiB: 128, Pids: 64}, Network: contract.Network{Mode: "none"}, AllowedTools: []string{"files.read", "files.write", "process.exec"}},
		}, Limits: contract.Limits{Timeout: "30m", MaxConcurrentNodes: 8, MaxNodeInstances: 10000, MaxModelCalls: 200, MaxToolCalls: 1000},
	}}
}

// WriteOnce preserves editable packages, profiles and durable identifiers across
// restarts. A different provider/port setup requires a separate directory.
func (s *Setup) WriteOnce() (settings, error) {
	if err := s.Validate(); err != nil {
		return settings{}, err
	}
	if err := os.MkdirAll(s.Config.Dir, 0700); err != nil {
		return settings{}, err
	}
	file := filepath.Join(s.Config.Dir, "settings.json")
	var state settings
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return state, err
		}
		initial := settings{Config: s.Config, Password: hex.EncodeToString(random[:16]), OperationID: hex.EncodeToString(random[16:])}
		encoded, _ := json.MarshalIndent(initial, "", "  ")
		if err = writeNew(file, encoded); err != nil {
			return state, err
		}
		// Read the winning settings if another quickstart initialized concurrently.
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid quickstart settings: %w", err)
	}
	if state.Config.Provider != s.Config.Provider || state.Config.Model != s.Config.Model || state.Config.PostgresPort != s.Config.PostgresPort {
		return state, fmt.Errorf("this directory uses a different provider, model or ports; reuse its flags or choose another --dir")
	}
	if state.Password == "" || state.OperationID == "" {
		return state, fmt.Errorf("quickstart settings have no password or operation identity")
	}
	if err := writeNew(filepath.Join(s.Config.Dir, "compose.yaml"), assets.Compose); err != nil {
		return state, err
	}
	profile, err := json.MarshalIndent(s.profile(), "", "  ")
	if err != nil {
		return state, err
	}
	if err = writeNew(filepath.Join(s.Config.Dir, "profile.json"), profile); err != nil {
		return state, err
	}
	if len(s.helper) > 0 {
		sum := sha256.Sum256(s.helper)
		helperPath := filepath.Join(s.Config.Dir, "helpers", "sandbox-helper-"+hex.EncodeToString(sum[:]))
		if err = writeNewMode(helperPath, s.helper, 0755); err != nil {
			return state, err
		}
		actual, err := os.ReadFile(helperPath)
		if err != nil {
			return state, err
		}
		if !bytes.Equal(actual, s.helper) {
			return state, fmt.Errorf("saved sandbox helper was modified: %s", helperPath)
		}
		// The sandbox runs as UID 65532, so the public helper must be executable
		// by a different user after it is bind-mounted by the Docker daemon.
		if err = os.Chmod(helperPath, 0755); err != nil {
			return state, err
		}
		s.Config.HelperPath = helperPath
	}
	err = fs.WalkDir(starter.Files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := starter.Files.ReadFile(name)
		if err != nil {
			return err
		}
		return writeNew(filepath.Join(s.Config.Dir, "examples", filepath.FromSlash(name)), data)
	})
	return state, err
}

func writeNew(name string, data []byte) error {
	return writeNewMode(name, data, 0600)
}

func writeNewMode(name string, data []byte, mode os.FileMode) error {
	if _, err := os.Lstat(name); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".knotra-create-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Link is atomic and cannot overwrite edits made by another process.
	err = os.Link(file.Name(), name)
	if os.IsExist(err) {
		return nil
	}
	return err
}

func (s *Setup) StartInfrastructure(ctx context.Context) (string, string, error) {
	state, err := s.WriteOnce()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(s.Config.Dir))
	args := []string{"--project-name", "knotra-quickstart-" + hex.EncodeToString(sum[:6]), "--file", filepath.Join(s.Config.Dir, "compose.yaml"), "up", "-d", "--wait", "--wait-timeout", "120"}
	if s.compose == "docker" {
		args = append([]string{"compose"}, args...)
	}
	env := []string{
		"KNOTRA_DEV_DB_PASSWORD=" + state.Password,
		fmt.Sprintf("KNOTRA_PG_PORT=%d", s.Config.PostgresPort),
	}
	_, _ = fmt.Fprintln(s.Out, "Starting persistent PostgreSQL development service…")
	if _, err = s.run(ctx, s.compose, args, env); err != nil {
		return "", "", fmt.Errorf("infrastructure startup failed; check port conflicts and Docker logs: %w", err)
	}
	for _, image := range []string{"python:3.13-alpine", "node:22-alpine"} {
		if _, err = s.run(ctx, "docker", []string{"image", "inspect", image}, nil); err != nil {
			_, _ = fmt.Fprintf(s.Out, "Downloading %s…\n", image)
			if _, err = s.run(ctx, "docker", []string{"pull", image}, nil); err != nil {
				return "", "", err
			}
		}
	}
	if s.Config.Provider == "ollama" && !s.modelInstalled {
		_, _ = fmt.Fprintf(s.Out, "Ensuring model %s is installed…\n", s.Config.Model)
		if _, err = s.run(ctx, "ollama", []string{"pull", s.Config.Model}, nil); err != nil {
			return "", "", err
		}
	}
	return fmt.Sprintf("postgres://knotra:%s@127.0.0.1:%d/knotra?sslmode=disable", state.Password, s.Config.PostgresPort), state.OperationID, nil
}
