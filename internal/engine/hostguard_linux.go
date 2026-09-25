package engine

import (
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

// guardHost stops containers from opening connections to the host itself,
// such as SSH or a database listening on all addresses. Replies to
// connections the host started, like the proxy talking to an app, still get
// through.
//
// The rules live in a table of their own, so they work next to ufw, firewalld
// or hand-written rules without touching them. The table is replaced as a
// whole in one transaction, which makes this safe to run on every start.
func guardHost() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: "zelie"}
	c.AddTable(table) // so the delete below never fails on a fresh machine
	c.DelTable(table)
	c.AddTable(table)

	policy := nftables.ChainPolicyAccept
	input := c.AddChain(&nftables.Chain{
		Name:     "input",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policy,
	})

	// Matching the first bytes of the name is how nft's "zelie*" works.
	fromBridge := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("zelie")},
	}
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: append(fromBridge[:len(fromBridge):len(fromBridge)],
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	)})
	c.AddRule(&nftables.Rule{Table: table, Chain: input, Exprs: append(fromBridge[:len(fromBridge):len(fromBridge)],
		&expr.Verdict{Kind: expr.VerdictDrop},
	)})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("install host firewall rules: %w", err)
	}
	return nil
}
