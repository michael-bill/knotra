//go:build !windows

package adapters

import "fmt"

const defaultDockerHost = "unix:///var/run/docker.sock"

func namedPipeDialer(string) (dockerDialFunc, error) {
	return nil, fmt.Errorf("Docker named pipes require Windows")
}
