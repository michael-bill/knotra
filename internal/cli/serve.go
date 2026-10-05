package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/michael-bill/knotra/internal/app"
)

func (s *commandState) serveCommand() *cobra.Command {
	o := app.Options{Version: s.options.Version}
	var debug bool
	cmd := &cobra.Command{Use: "serve", Short: "Run the HTTP engine and Temporal worker", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if o.DatabaseURL == "" {
			return fmt.Errorf("--database-url or KNOTRA_DATABASE_URL is required")
		}
		if len(o.Profiles) == 0 {
			return fmt.Errorf("at least one --profile EngineProfile.yaml is required")
		}
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		o.Token = s.options.Getenv("KNOTRA_TOKEN")
		log := slog.New(slog.NewJSONHandler(s.options.Err, &slog.HandlerOptions{Level: level}))
		return app.Serve(cmd.Context(), o, log)
	}}
	f := cmd.Flags()
	f.StringVar(&o.Listen, "listen", s.env("KNOTRA_LISTEN", "127.0.0.1:8787"), "HTTP listen address")
	f.StringVar(
		&o.DatabaseURL,
		"database-url",
		s.env("KNOTRA_DATABASE_URL", ""),
		"PostgreSQL URL (prefer KNOTRA_DATABASE_URL for credentials)",
	)
	f.StringVar(
		&o.TemporalAddress,
		"temporal-address",
		s.env("KNOTRA_TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		"Temporal frontend address",
	)
	f.StringVar(&o.Namespace, "namespace", s.env("KNOTRA_TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	f.StringVar(&o.TaskQueue, "task-queue", s.env("KNOTRA_TASK_QUEUE", "knotra"), "Temporal task queue")
	f.StringVar(
		&o.DataDir,
		"data-dir",
		s.env("KNOTRA_DATA_DIR", ".knotra"),
		"Engine payload, artifact and sandbox directory",
	)
	f.StringArrayVar(&o.Profiles, "profile", nil, "Trusted EngineProfile YAML file (repeatable)")
	f.StringVar(
		&o.DockerHost,
		"docker-host",
		s.env("DOCKER_HOST", ""),
		"Docker local unix socket or named pipe URL; otherwise current Docker context",
	)
	f.StringVar(
		&o.HelperPath,
		"sandbox-helper",
		s.env("KNOTRA_SANDBOX_HELPER", ".knotra/bin/sandbox-helper"),
		"Trusted Linux sandbox helper executable",
	)
	f.StringVar(
		&o.FirewallImage,
		"firewall-image",
		s.env("KNOTRA_FIREWALL_IMAGE", "knotra-firewall:dev"),
		"Trusted firewall helper image for allowlist networks",
	)
	f.StringVar(&o.CORSOrigin, "cors-origin", s.env("KNOTRA_CORS_ORIGIN", ""), "Exact permitted browser origin")
	f.StringVar(&o.TLSCert, "tls-cert", s.env("KNOTRA_TLS_CERT", ""), "TLS certificate file")
	f.StringVar(&o.TLSKey, "tls-key", s.env("KNOTRA_TLS_KEY", ""), "TLS private key file")
	f.BoolVar(&debug, "debug", false, "Enable debug logs")
	return cmd
}
