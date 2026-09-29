package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// procNet lists what listens or is bound, by protocol and address family.
// Only the core can see all of it: /proc/net/tcp shows every socket to
// anyone, but the panel runs where it cannot be sure to.
var procNet = []string{"/proc/net/tcp", "/proc/net/tcp6", "/proc/net/udp", "/proc/net/udp6"}

// UsedPorts returns the ports something on this machine holds: TCP ports
// that listen and UDP ports that are bound, on any address. A range for
// game servers is chosen from what is left.
func UsedPorts() ([]int, error) {
	seen := map[int]bool{}
	var errs []error
	found := false
	for _, path := range procNet {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			// A machine without IPv6 has no tcp6.
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		found = true
		err = readPorts(f, strings.Contains(path, "tcp"), seen)
		f.Close()
		if err != nil {
			errs = append(errs, err)
		}
	}
	if !found {
		return nil, errors.Join(append(errs, errors.New("read /proc/net: no socket tables"))...)
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	slices.Sort(ports)
	return ports, errors.Join(errs...)
}

// readPorts adds the local ports of one /proc/net table to seen. Lines look
// like "0: 0100007F:1F90 00000000:0000 0A ...": the port is the hex number
// after the colon in the second field, and 0A is the state of a TCP socket
// that listens.
func readPorts(r io.Reader, tcp bool, seen map[int]bool) error {
	sc := bufio.NewScanner(r)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if tcp && fields[3] != "0A" {
			continue
		}
		_, hex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hex, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		seen[int(port)] = true
	}
	return sc.Err()
}

func (s *Server) usedPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := UsedPorts()
	if err != nil {
		s.fail(w, "list used ports", "", err)
		return
	}
	writeJSON(w, http.StatusOK, ports)
}

// UsedPorts returns the ports in use on the server, sorted.
func (c *Client) UsedPorts(ctx context.Context) ([]int, error) {
	var ports []int
	err := c.do(ctx, http.MethodGet, "/v1/ports", nil, &ports)
	return ports, err
}
