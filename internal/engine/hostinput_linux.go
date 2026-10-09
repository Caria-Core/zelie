package engine

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// Zelie's own nftables table accepts the DNS questions containers ask their
// gateway, but an accept in one table does not overrule a drop in another.
// ufw, and firewalls like it, drop what reaches the host unless their own
// iptables chains allow it. So the same opening is also made there, in a
// chain of Zelie's own that INPUT jumps to first, the way Docker and the CNI
// plugins add theirs: only DNS, only from Zelie's bridges, and port 3306 on
// the gateway of an app that has host access.
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

// ensureHostInput makes sure INPUT lets containers ask their DNS server, and
// lets the apps with host access reach the host's database port. Without
// iptables on the machine there is nothing that could drop it. With rewrite
// the chain is emptied and filled again, which is how an opening that went
// away is closed; without it, missing rules are added.
func ensureHostInput(ctx context.Context, host []hostRule, rewrite bool) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	return ensureInputRules(ctx, host, rewrite)
}

func ensureInputRules(ctx context.Context, host []hostRule, rewrite bool) error {
	if _, err := iptables(ctx, "-L", inputChain, "-n"); err != nil {
		if out, err := iptables(ctx, "-N", inputChain); err != nil {
			return fmt.Errorf("iptables -N %s: %v: %s", inputChain, err, strings.TrimSpace(out))
		}
	} else if rewrite {
		if out, err := iptables(ctx, "-F", inputChain); err != nil {
			return fmt.Errorf("iptables -F %s: %v: %s", inputChain, err, strings.TrimSpace(out))
		}
	}
	rules := slices.Clone(inputRules)
	for _, h := range host {
		rules = append(rules, h.iptablesRule())
	}
	for _, rule := range rules {
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

// Forwarded ports need the same for FORWARD: ufw's policy there is to drop,
// so the connections Zelie sends to a container are accepted in a chain of
// Zelie's own, one rule per forwarded port.
const forwardChain = "ZELIE-FORWARD"

// ensureHostForward opens the forwarded ports in the host's iptables. With
// rewrite the chain is emptied and filled again, which is how a port that
// went away or moved to another address is closed; without it, missing
// rules are added and the jump is put back if a firewall reload took it.
func ensureHostForward(ctx context.Context, list []portMap, rewrite bool) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	return ensureForwardRules(ctx, list, rewrite)
}

func ensureForwardRules(ctx context.Context, list []portMap, rewrite bool) error {
	_, err := iptables(ctx, "-L", forwardChain, "-n")
	if err != nil {
		if len(list) == 0 {
			// Nothing was ever opened here.
			return nil
		}
		if out, err := iptables(ctx, "-N", forwardChain); err != nil {
			return fmt.Errorf("iptables -N %s: %v: %s", forwardChain, err, strings.TrimSpace(out))
		}
	} else if rewrite {
		if out, err := iptables(ctx, "-F", forwardChain); err != nil {
			return fmt.Errorf("iptables -F %s: %v: %s", forwardChain, err, strings.TrimSpace(out))
		}
	}
	for _, f := range list {
		rule := forwardRule(f)
		if !rewrite {
			if _, err := iptables(ctx, append([]string{"-C", forwardChain}, rule...)...); err == nil {
				continue
			}
		}
		if out, err := iptables(ctx, append([]string{"-A", forwardChain}, rule...)...); err != nil {
			return fmt.Errorf("iptables -A %s: %v: %s", forwardChain, err, strings.TrimSpace(out))
		}
	}
	if _, err := iptables(ctx, "-C", "FORWARD", "-j", forwardChain); err == nil {
		return nil
	}
	if out, err := iptables(ctx, "-I", "FORWARD", "1", "-j", forwardChain); err != nil {
		return fmt.Errorf("iptables -I FORWARD: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}
