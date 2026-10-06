package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/michael-bill/knotra/internal/app"
	"github.com/michael-bill/knotra/internal/bootstrap"
	"github.com/michael-bill/knotra/internal/protocol"
)

func (s *commandState) bootstrapCommand(checkOnly bool) *cobra.Command {
	config := bootstrap.Config{Provider: "ollama", PostgresPort: 25432}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	config.Dir = filepath.Join(dir, "knotra", "quickstart")
	name, short := "quickstart", "Prepare persistent local services, run an example and serve the engine"
	if checkOnly {
		name, short = "doctor", "Check Docker, Compose, model access and sandbox helper"
	}
	var listen, cors string
	var noRun bool
	cmd := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if s.json {
			return fmt.Errorf("%s prints setup progress; omit --json", name)
		}
		if config.Model == "" && config.Provider == "ollama" {
			config.Model = "qwen3.5:9b"
		}
		setup := &bootstrap.Setup{Config: config, Out: s.options.Out, Getenv: s.options.Getenv}
		checkCtx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		err := setup.Doctor(checkCtx)
		cancel()
		if err != nil || checkOnly {
			return err
		}
		return s.quickstart(cmd.Context(), setup, listen, cors, noRun)
	}}
	f := cmd.Flags()
	f.StringVar(&config.Dir, "dir", config.Dir, "Persistent quickstart directory (keep it to retain runs and artifacts)")
	f.StringVar(&config.Provider, "provider", config.Provider, "Model provider: ollama, openai or anthropic")
	f.StringVar(&config.Model, "model", "", "Model ID; defaults to qwen3.5:9b for Ollama, required for cloud providers")
	f.StringVar(&config.HelperPath, "sandbox-helper", s.env("KNOTRA_SANDBOX_HELPER", ""), "Linux helper; otherwise select it from the release bundle")
	f.IntVar(&config.PostgresPort, "postgres-port", config.PostgresPort, "Local PostgreSQL port")
	if !checkOnly {
		f.StringVar(&listen, "listen", "127.0.0.1:8787", "Loopback engine listen address")
		f.StringVar(&cors, "cors-origin", "", "Exact browser origin when using the development UI")
		f.BoolVar(&noRun, "no-run", false, "Start the engine without submitting the greeting example")
	}
	return cmd
}

func (s *commandState) quickstart(ctx context.Context, setup *bootstrap.Setup, listen, cors string, noRun bool) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid --listen: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("quickstart requires a loopback listen address")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("engine address %s is occupied; stop that engine or choose --listen", listen)
	}
	_ = listener.Close()
	dsn, key, err := setup.StartInfrastructure(ctx)
	if err != nil {
		return err
	}
	config := setup.Config
	logfile, err := os.OpenFile(filepath.Join(config.Dir, "engine.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = logfile.Close() }()
	serviceCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- app.Serve(serviceCtx, app.Options{
			Listen: listen, DatabaseURL: dsn,
			Namespace: "default", TaskQueue: "knotra", DataDir: filepath.Join(config.Dir, "engine"),
			HelperPath: config.HelperPath, Profiles: []string{filepath.Join(config.Dir, "profile.json")},
			Version: s.options.Version, CORSOrigin: cors,
		}, slog.New(slog.NewJSONHandler(logfile, nil)))
		close(done)
	}()
	defer func() { cancel(); <-done }()
	s.endpoint = "http://" + listen
	s.stateDir = filepath.Join(config.Dir, "client")
	readyCtx, stopReady := context.WithTimeout(ctx, 45*time.Second)
	err = s.waitQuickstart(readyCtx, done)
	stopReady()
	if err != nil {
		return fmt.Errorf("engine startup failed (see %s): %w", filepath.Join(config.Dir, "engine.log"), err)
	}
	_, _ = fmt.Fprintf(s.options.Out, "Engine: %s\nData and editable examples: %s\n", s.endpoint, config.Dir)
	if !noRun {
		if err = s.quickstartExample(ctx, config.Dir, key); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(s.options.Out, "Ready for your pipelines. Connect Desktop to %s.\nCtrl-C stops the engine; rerun this command to continue saved runs.\n", s.endpoint)
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}

func (s *commandState) waitQuickstart(ctx context.Context, done <-chan error) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err == nil {
				return fmt.Errorf("engine stopped before becoming ready")
			}
			return err
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			var info map[string]any
			err := s.client().Get(probeCtx, "/info", &info)
			cancel()
			if err == nil {
				return nil
			}
		}
	}
}

func (s *commandState) quickstartExample(ctx context.Context, dir, key string) error {
	pkg, err := loadPackage(filepath.Join(dir, "examples", "hello", "pipeline.yaml"), "")
	if err != nil {
		return err
	}
	var definition struct {
		Definition protocol.Definition `json:"definition"`
	}
	if err = s.client().Command(ctx, "/definitions", map[string]any{"package": pkg}, definitionKey(key), &definition); err != nil {
		return err
	}
	var accepted struct {
		Run protocol.Run `json:"run"`
	}
	if err = s.client().Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": "local", "inputs": map[string]any{}, "artifacts": map[string]any{}}, key, &accepted); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.options.Out, "Greeting run: %s\n", accepted.Run.ID)
	run, err := s.waitRun(ctx, accepted.Run.ID)
	if err != nil {
		return err
	}
	if err = runExit(run); err != nil {
		return err
	}
	for _, artifact := range run.Artifacts {
		if artifact.Name != "greeting.txt" {
			continue
		}
		output := filepath.Join(dir, "greeting.txt")
		if existing, err := os.ReadFile(output); err == nil {
			data, err := s.client().Bytes(ctx, "/artifacts/"+artifact.ID+"/content")
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, data) {
				return fmt.Errorf("%s was edited; preserve or move it before downloading the saved greeting", output)
			}
			_, _ = fmt.Fprintf(s.options.Out, "Saved greeting already exists: %s\n", output)
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		return s.download(ctx, artifact.ID, output, false)
	}
	return fmt.Errorf("completed greeting run has no greeting.txt artifact")
}
