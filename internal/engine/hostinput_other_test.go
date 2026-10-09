//go:build !linux

package engine

import "testing"

func quietIptables(*testing.T) {}
