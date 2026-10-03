//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package client

import (
	"context"
	"errors"
)

func (c *Client) lockCommand(context.Context, string) (func(), error) {
	return nil, errors.New("durable CLI command journal requires a supported Unix platform")
}
