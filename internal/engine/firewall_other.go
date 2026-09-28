//go:build !linux

package engine

import "errors"

func applyFirewall(*firewall) error { return errors.New("the host firewall needs Linux") }

func deleteLink(string) error { return nil }
