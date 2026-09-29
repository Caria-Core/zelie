package egg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Entry is one egg in the curated list. The list is not embedded in the
// binary: it names files in the pelican-eggs repositories (MIT)
// at a fixed commit, and the commits move with Zelie releases.
type Entry struct {
	ID string
	// Kind tells the eggs of games from the generic ones that run a
	// program of the user's own. The two are offered in different places.
	Kind   Kind
	Name   string
	Game   string
	Repo   string // owner/name on GitHub
	Commit string // full SHA
	Path   string

	// What a game needs when the egg does not say, offered as the defaults
	// of a new server: memory and disk in MB, and how many ports it takes.
	// Zero means the general defaults.
	MemoryMB int64
	DiskMB   int64
	Ports    int
}

// Kind is what an egg is for.
type Kind string

const (
	// KindGame is the zero value: a game server.
	KindGame Kind = ""
	// KindRuntime is a generic egg that runs files the user brings, such
	// as a Node.js or Python program.
	KindRuntime Kind = "runtime"
)

var Catalog = []Entry{
	{
		ID: "minecraft-paper", Name: "Paper", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: "75bf05db3c6c305e0fa6eef1d38c7e7176121de9",
		Path:   "java/paper/egg-paper.yaml",
	},
	{
		ID: "rust", Name: "Rust", Game: "Rust",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: "e17e2c3db36aaf1ddecbc227803dd6cdbb0e6b1f",
		Path:   "rust/vanilla/egg-rust.yaml",
		// The game files alone are about 10 GB, and the server holds the
		// whole map in memory. It listens on four ports: the game's, the
		// query, RCON and the companion app.
		MemoryMB: 8 << 10,
		DiskMB:   30 << 10,
		Ports:    4,
	},
	{
		ID: "palworld", Name: "Palworld", Game: "Palworld",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: "e17e2c3db36aaf1ddecbc227803dd6cdbb0e6b1f",
		Path:   "palworld/egg-palworld.yaml",
	},

	// Generic eggs from pelican-eggs/generic. They start whatever the
	// files in the volume are, so each takes the runtime's own name.
	{
		ID: "nodejs", Kind: KindRuntime, Name: "Node.js", Game: "Node.js",
		Repo:   "pelican-eggs/generic",
		Commit: genericCommit,
		Path:   "nodejs/egg-nodejs-generic.yaml",
	},
	{
		ID: "python", Kind: KindRuntime, Name: "Python", Game: "Python",
		Repo:   "pelican-eggs/generic",
		Commit: genericCommit,
		Path:   "python/egg-python-generic.json",
	},
	{
		ID: "bun", Kind: KindRuntime, Name: "Bun", Game: "Bun",
		Repo:   "pelican-eggs/generic",
		Commit: genericCommit,
		Path:   "bun/egg-bun.json",
	},
	{
		ID: "deno", Kind: KindRuntime, Name: "Deno", Game: "Deno",
		Repo:   "pelican-eggs/generic",
		Commit: genericCommit,
		Path:   "deno/egg-deno-generic.json",
	},
	// The generic Go egg is left out: it fetches and builds a remote package
	// with GOPATH-era tooling instead of running the files the app has.
	{
		ID: "java", Kind: KindRuntime, Name: "Java", Game: "Java",
		Repo:   "pelican-eggs/generic",
		Commit: genericCommit,
		Path:   "java/egg-generic-java.json",
		// The JVM wants room beside its heap.
		MemoryMB: 1024,
	},
}

const genericCommit = "18aeeb4bb54e04ccaf3410623afcccaec5efab82"

// Lookup returns the catalog entry with the given id.
func Lookup(id string) (Entry, bool) {
	for _, e := range Catalog {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// OfKind lists the catalog entries of one kind, in catalog order.
func OfKind(k Kind) []Entry {
	var out []Entry
	for _, e := range Catalog {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// RawBase is where catalog files are downloaded from. Tests point it at a
// local server.
var RawBase = "https://raw.githubusercontent.com"

// Eggs are a few tens of kilobytes; the largest in the catalog carries an
// embedded icon and is under 20 KiB.
const maxSize = 1 << 20

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// publicClient is what FetchURL uses when it is given no client. The address
// comes from a user, and the panel should not become a way to reach services
// on the server itself or its private network.
var publicClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

func publicOnly(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := ap.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return fmt.Errorf("%s is not a public address", ip)
	}
	return nil
}

// Fetch downloads a catalog entry and returns it parsed, along with the
// file as downloaded so it can be stored unchanged.
func Fetch(ctx context.Context, client *http.Client, e Entry) (*Egg, []byte, error) {
	u := RawBase + "/" + e.Repo + "/" + e.Commit + "/" + e.Path
	return fetch(ctx, client, u)
}

// FetchURL downloads an egg from any https address. Without a client of
// its own it only connects to public addresses.
func FetchURL(ctx context.Context, client *http.Client, address string) (*Egg, []byte, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return nil, nil, errors.New("not a valid address")
	}
	if u.Scheme != "https" {
		return nil, nil, errors.New("egg address must use https")
	}
	if client == nil {
		client = publicClient
	}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("redirected away from https")
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return fetch(ctx, &c, address)
}

func fetch(ctx context.Context, client *http.Client, address string) (*Egg, []byte, error) {
	if client == nil {
		client = defaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("download %s: %s", host(address), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxSize {
		return nil, nil, fmt.Errorf("egg is larger than %d KiB", maxSize>>10)
	}
	e, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return e, data, nil
}

func host(address string) string {
	if u, err := url.Parse(address); err == nil && u.Host != "" {
		return u.Host + strings.TrimSuffix(u.Path, "/")
	}
	return address
}
