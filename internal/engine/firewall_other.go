//go:build !linux

package engine

import (
	"context"
	"errors"
)

func applyFirewall(*firewall) error { return errors.New("the host firewall needs Linux") }

func deleteLink(string) error { return nil }

func ensureHostInput(context.Context) error { return nil }

func ensureHostForward(context.Context, []portMap, bool) error { return nil }
