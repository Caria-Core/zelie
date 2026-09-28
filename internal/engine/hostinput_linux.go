package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Zelie's own nftables table accepts the DNS questions containers ask their
// gateway, but an accept in one table does not overrule a drop in another.
// ufw, and firewalls like it, drop what reaches the host unless their own
// iptables chains allow it. So the same opening is also made there, in a
// chain of Zelie's own that INPUT jumps to first, the way Docker and the CNI
// plugins add theirs: only DNS, only from Zelie's bridges.
const inputChain = "ZELIE-INPUT"

var inputRules = [][]string{
	{"-i", "zelie+", "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
	{"-i", "zelie+", "-p", "tcp", "--dport", "53", "-j", "ACCEPT"},
}

// iptables runs the host's iptables, whichever backend it uses. Tests
// replace it.
var iptables = func(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
	return string(out), err
}

// ensureHostInput makes sure INPUT lets containers ask their DNS server.
// Without iptables on the machine there is nothing that could drop it.
func ensureHostInput(ctx context.Context) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	return ensureInputRules(ctx)
}

func ensureInputRules(ctx context.Context) error {
	if _, err := iptables(ctx, "-L", inputChain, "-n"); err != nil {
		if out, err := iptables(ctx, "-N", inputChain); err != nil {
			return fmt.Errorf("iptables -N %s: %v: %s", inputChain, err, strings.TrimSpace(out))
		}
	}
	for _, rule := range inputRules {
		if _, err := iptables(ctx, append([]string{"-C", inputChain}, rule...)...); err == nil {
			continue
		}
		if out, err := iptables(ctx, append([]string{"-A", inputChain}, rule...)...); err != nil {
			return fmt.Errorf("iptables -A %s: %v: %s", inputChain, err, strings.TrimSpace(out))
		}
	}
	if _, err := iptables(ctx, "-C", "INPUT", "-j", inputChain); err == nil {
		return nil
	}
	if out, err := iptables(ctx, "-I", "INPUT", "1", "-j", inputChain); err != nil {
		return fmt.Errorf("iptables -I INPUT: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}
