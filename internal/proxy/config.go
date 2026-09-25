// Package proxy is Zelie's web front door. It terminates HTTPS, gets
// certificates automatically, and forwards each domain to the container that
// serves it. It runs as its own unprivileged process so the panel can restart
// or update without dropping a single site.
package proxy

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/Caria-Core/zelie/internal/engine"
)

// TLS modes.
const (
	TLSACME       = "acme"        // certificates from Let's Encrypt
	TLSSelfSigned = "self-signed" // for machines no certificate authority can reach
)

// Route sends requests for Host to Upstream, a container address.
type Route struct {
	Host     string `json:"host"`
	Upstream string `json:"upstream"`
}

// Config is everything the proxy serves. It is always replaced as a whole.
type Config struct {
	TLS    string  `json:"tls"`
	Email  string  `json:"email,omitempty"` // for the certificate authority
	Routes []Route `json:"routes"`
}

// upstreams is where routes may point. Tests widen it to reach a local server.
var upstreams = engine.NetworkRange

var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Validate rejects anything the proxy should not serve. Upstreams must be
// container addresses: the proxy is never a way to reach the host or the
// wider network.
func (c *Config) Validate() error {
	switch c.TLS {
	case "":
		c.TLS = TLSACME
	case TLSACME, TLSSelfSigned:
	default:
		return fmt.Errorf("unknown tls mode %q", c.TLS)
	}
	seen := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		host := strings.ToLower(strings.TrimSuffix(r.Host, "."))
		c.Routes[i].Host = host
		if seen[host] {
			return fmt.Errorf("%s is routed twice", host)
		}
		seen[host] = true

		ip, err := netip.ParseAddr(host)
		switch {
		case err == nil && c.TLS == TLSACME:
			// Let's Encrypt issues IP certificates only through a separate
			// short-lived profile, which is not wired up yet.
			return fmt.Errorf("%s: IP addresses need tls mode %q for now", host, TLSSelfSigned)
		case err == nil && !ip.IsGlobalUnicast():
			return fmt.Errorf("%s is not a usable address", host)
		case err != nil && !hostname.MatchString(host):
			return fmt.Errorf("%q is not a valid domain name", r.Host)
		}

		up, err := netip.ParseAddrPort(r.Upstream)
		if err != nil {
			return fmt.Errorf("upstream of %s: %w", host, err)
		}
		if !upstreams.Contains(up.Addr()) || up.Port() == 0 {
			return fmt.Errorf("upstream of %s must be a container address in %s", host, upstreams)
		}
	}
	if c.TLS == TLSACME && len(c.Routes) > 0 && c.Email == "" {
		return errors.New("an email address is needed for certificates")
	}
	return nil
}
