package core

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// PublicAddress is the address players are most likely to reach this
// machine on. Private means no network card has a public one, so players
// outside the machine's network may not get through.
type PublicAddress struct {
	Address string `json:"address"`
	Private bool   `json:"private"`
}

// ifaceAddr is one IPv4 address on a network card.
type ifaceAddr struct {
	Name string
	Addr netip.Addr
}

// ownInterfaces are the cards Zelie and the container runtimes make for
// their own traffic. An address on one of them is never the machine's.
var ownInterfaces = []string{"lo", "zelie", "docker", "veth", "br-", "cni", "flannel", "cali", "virbr"}

// cgnat is the range carriers use for customers behind shared addresses
// (RFC 6598). It is not reachable from the internet.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func ownInterface(name string) bool {
	for _, p := range ownInterfaces {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// pickAddress chooses the address to show players: the first public IPv4
// one. Failing that, the first private one on the card the default route
// leaves by, or on any card when the route is unknown.
func pickAddress(addrs []ifaceAddr, defaultRoute string) (PublicAddress, bool) {
	var fallback, any netip.Addr
	for _, a := range addrs {
		ip := a.Addr.Unmap()
		if ownInterface(a.Name) || !ip.Is4() || !ip.IsGlobalUnicast() {
			continue
		}
		if !ip.IsPrivate() && !cgnat.Contains(ip) {
			return PublicAddress{Address: ip.String()}, true
		}
		if !any.IsValid() {
			any = ip
		}
		if a.Name == defaultRoute && !fallback.IsValid() {
			fallback = ip
		}
	}
	if defaultRoute == "" {
		fallback = any
	}
	if !fallback.IsValid() {
		return PublicAddress{}, false
	}
	return PublicAddress{Address: fallback.String(), Private: true}, true
}

// defaultRouteCard reads the card the default route leaves by from a
// /proc/net/route table. Its lines are "eth0 00000000 0100A8C0 0003 ...":
// a destination and mask of zero, with the route up (flag 1), is the
// default one.
func defaultRouteCard(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Scan() // the header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		if flags, err := strconv.ParseUint(f[3], 16, 32); err == nil && flags&1 == 1 {
			return f[0]
		}
	}
	return ""
}

// DetectAddress reads the machine's network cards. Nothing is asked of
// any outside service, so it works the same behind a tunnel or offline.
func DetectAddress() (PublicAddress, error) {
	cards, err := net.Interfaces()
	if err != nil {
		return PublicAddress{}, err
	}
	var addrs []ifaceAddr
	for _, c := range cards {
		if c.Flags&net.FlagUp == 0 {
			continue
		}
		list, err := c.Addrs()
		if err != nil {
			continue
		}
		for _, a := range list {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				addrs = append(addrs, ifaceAddr{Name: c.Name, Addr: p.Addr()})
			}
		}
	}
	route := ""
	if f, err := os.Open("/proc/net/route"); err == nil {
		route = defaultRouteCard(f)
		f.Close()
	}
	// None found is an empty address: the interface falls back to the name
	// the panel was opened with.
	got, _ := pickAddress(addrs, route)
	return got, nil
}

func (s *Server) publicAddress(w http.ResponseWriter, r *http.Request) {
	a, err := DetectAddress()
	if err != nil {
		s.fail(w, "detect address", "", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// PublicAddress returns the address found on the machine's network cards.
func (c *Client) PublicAddress(ctx context.Context) (PublicAddress, error) {
	var a PublicAddress
	err := c.do(ctx, http.MethodGet, "/v1/address", nil, &a)
	return a, err
}
