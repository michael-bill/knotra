package adapters

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

const defaultDockerHost = "npipe:////./pipe/docker_engine"

func namedPipeDialer(path string) (dockerDialFunc, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, path)
	}, nil
}
