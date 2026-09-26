//go:build !linux

package engine

import "net"

func freebind() net.ListenConfig { return net.ListenConfig{} }
