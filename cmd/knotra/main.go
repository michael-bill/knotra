package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/michael-bill/knotra/internal/cli"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, os.Args[1:], cli.Options{Version: version})
	cancel()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(cli.ExitCode(err))
}
