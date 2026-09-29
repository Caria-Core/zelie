package core

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000   109        0 23456 1 0000000000000000 100 0 0 10 0
   2: 0F02000A:0016 C0A80001:D3E4 01 00000000:00000000 02:000A2C39 00000000     0        0 34567 2 0000000000000000 20 4 30 10 -1
`

const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 45678 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:9C41 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 56789 1 0000000000000000 100 0 0 10 0
`

const procUDP = `   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
  100: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000   101        0 11111 2 0000000000000000 0
  200: 0F02000A:0044 0100000A:0043 01 00000000:00000000 00:00000000 00000000     0        0 22222 2 0000000000000000 0
`

func TestReadPorts(t *testing.T) {
	seen := map[int]bool{}
	for _, c := range []struct {
		table string
		tcp   bool
	}{{procTCP, true}, {procTCP6, true}, {procUDP, false}} {
		if err := readPorts(strings.NewReader(c.table), c.tcp, seen); err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	for p := range seen {
		got = append(got, p)
	}
	sort.Ints(got)
	// 22 only listens in the IPv6 table: in the IPv4 one it is an
	// established connection. UDP counts whatever the state.
	want := []int{22, 53, 68, 3306, 8080, 40001}
	if !slices.Equal(got, want) {
		t.Errorf("ports %v, want %v", got, want)
	}
}

func TestReadPortsIgnoresJunk(t *testing.T) {
	seen := map[int]bool{}
	junk := "header\nshort line\n  0: nocolon 00000000:0000 0A\n  1: 00000000:ZZZZ 00000000:0000 0A\n  2: 00000000:0000 00000000:0000 0A\n"
	if err := readPorts(strings.NewReader(junk), true, seen); err != nil || len(seen) != 0 {
		t.Errorf("%v, %v", seen, err)
	}
}
