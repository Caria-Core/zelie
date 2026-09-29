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
	"sync"

	"github.com/Caria-Core/zelie/internal/engine"
)

// TLS modes.
const (
	TLSACME       = "acme"        // certificates from Let's Encrypt
	TLSSelfSigned = "self-signed" // for machines no certificate authority can reach
	// TLSTunnel is for a proxy behind a Cloudflare Tunnel: Cloudflare holds
	// the certificates, and the proxy serves plain HTTP on a loopback port
	// that only the tunnel's connector reaches.
	TLSTunnel = "tunnel"
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
	Panel  string  `json:"panel,omitempty"` // host name the panel answers on
	Routes []Route `json:"routes"`
}

// upstreams is where routes may point. Tests widen it to reach a local server.
var upstreams = engine.NetworkRange

var hostname = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
})

// ValidDomain reports whether h, in lower case, is a domain name the proxy
// can route.
func ValidDomain(h string) bool { return hostname().MatchString(h) }

// Validate rejects anything the proxy should not serve. Upstreams must be
// container addresses: apart from the panel's own socket, the proxy is never a
// way to reach the host or the wider network.
func (c *Config) Validate() error {
	switch c.TLS {
	case "":
		c.TLS = TLSACME
	case TLSACME, TLSSelfSigned, TLSTunnel:
	default:
		return fmt.Errorf("unknown tls mode %q", c.TLS)
	}
	seen := make(map[string]bool, len(c.Routes)+1)
	if c.Panel != "" {
		host, err := c.checkHost(c.Panel)
		if err != nil {
			return err
		}
		c.Panel = host
		seen[host] = true
	}
	for i, r := range c.Routes {
		host, err := c.checkHost(r.Host)
		if err != nil {
			return err
		}
		c.Routes[i].Host = host
		if seen[host] {
			return fmt.Errorf("%s is routed twice", host)
		}
		seen[host] = true

		up, err := netip.ParseAddrPort(r.Upstream)
		if err != nil {
			return fmt.Errorf("upstream of %s: %w", host, err)
		}
		if !upstreams.Contains(up.Addr()) || up.Port() == 0 {
			return fmt.Errorf("upstream of %s must be a container address in %s", host, upstreams)
		}
	}
	if c.TLS == TLSACME && len(seen) > 0 && c.Email == "" {
		return errors.New("an email address is needed for certificates")
	}
	return nil
}

// checkHost normalises a host name or IP address and checks that the proxy
// can serve it in the configured TLS mode.
func (c *Config) checkHost(h string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(h, "."))
	ip, err := netip.ParseAddr(host)
	switch {
	case err == nil && c.TLS == TLSACME:
		// Let's Encrypt issues IP certificates only through a separate
		// short-lived profile, which is not wired up yet.
		return "", fmt.Errorf("%s: IP addresses need tls mode %q for now", host, TLSSelfSigned)
	case err == nil && !ip.IsGlobalUnicast():
		return "", fmt.Errorf("%s is not a usable address", host)
	case err != nil && !hostname().MatchString(host):
		return "", fmt.Errorf("%q is not a valid domain name", h)
	}
	return host, nil
}
