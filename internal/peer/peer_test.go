package peer

import "testing"

func TestPolicy(t *testing.T) {
	p := Policy{UIDs: []uint32{999}}
	for uid, want := range map[uint32]bool{0: true, 999: true, 1000: false} {
		if got := p.Allows(Peer{UID: uid}); got != want {
			t.Errorf("uid %d: got %v, want %v", uid, got, want)
		}
	}
}
