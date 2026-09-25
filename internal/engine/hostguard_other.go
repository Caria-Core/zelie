//go:build !linux

package engine

import "errors"

func guardHost() error { return errors.New("the host firewall needs Linux") }
