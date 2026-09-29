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
	// Block is how many ports in a row, from the game port, the game itself
	// opens without an egg variable to say where (Valheim's query port is
	// always the game port plus one). They are part of Ports. Zero or one
	// means the game port stands alone.
	Block int
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
		ID: "minecraft-vanilla", Name: "Vanilla", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/vanilla/egg-vanilla-minecraft.yaml",
		// The jar and a world are a few hundred MB; the heap wants two GB for a few players.
		MemoryMB: 2 << 10,
		DiskMB:   5 << 10,
	},
	{
		ID: "minecraft-paper", Name: "Paper", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/paper/egg-paper.yaml",
	},
	{
		ID: "minecraft-purpur", Name: "Purpur", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/purpur/egg-purpur.yaml",
		// Paper with more settings, so it needs what Paper does.
		MemoryMB: 2 << 10,
		DiskMB:   5 << 10,
	},
	{
		ID: "minecraft-fabric", Name: "Fabric", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/fabric/egg-fabric.yaml",
		// Mods add to the heap, and a world grows with exploring.
		MemoryMB: 2 << 10,
		DiskMB:   5 << 10,
	},
	{
		ID: "minecraft-forge", Name: "Forge", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/forge/egg-forge-minecraft.yaml",
		// Forge mods and modpacks load a lot of content, and their worlds are larger.
		MemoryMB: 4 << 10,
		DiskMB:   10 << 10,
	},
	{
		ID: "minecraft-neoforge", Name: "NeoForge", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "java/neoforge/egg-neo-forge.json",
		// Same as Forge.
		MemoryMB: 4 << 10,
		DiskMB:   10 << 10,
	},
	{
		ID: "minecraft-velocity", Name: "Velocity", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "proxy/java/velocity/egg-velocity.json",
		// A proxy keeps no world; it only passes players on to other servers.
		MemoryMB: 512,
		DiskMB:   1 << 10,
	},
	{
		ID: "minecraft-bedrock", Name: "Bedrock", Game: "Minecraft",
		Repo:   "pelican-eggs/minecraft",
		Commit: minecraftCommit,
		Path:   "bedrock/bedrock/egg-vanilla-bedrock.json",
		// The native server is small; a world is tens of MB. Players connect over UDP.
		MemoryMB: 1 << 10,
		DiskMB:   2 << 10,
	},
	{
		ID: "7-days-to-die", Name: "7 Days to Die", Game: "7 Days to Die",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "7_days_to_die/egg-7-days-to-die.json",
		// The server files are about 12 GB, and the world generator wants 8 GB. It uses
		// the game port and the three after it (UDP). Telnet is only used from inside
		// the container, so its port is not opened.
		MemoryMB: 8 << 10,
		DiskMB:   20 << 10,
		Ports:    4,
		Block:    4,
	},
	{
		ID: "ark-survival-ascended", Name: "ARK: Survival Ascended", Game: "ARK: Survival Ascended",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "ark_survival_ascended/egg-a-r-k--survival-ascended.json",
		// The Windows server runs under Proton and holds a big map in memory:
		// 12 GB or more. The files are around 15 GB. RCON is only used from
		// inside the container, so its port is not opened.
		MemoryMB: 16 << 10,
		DiskMB:   40 << 10,
	},
	{
		ID: "counter-strike-2", Name: "Counter-Strike 2", Game: "Counter-Strike 2",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "counter_strike/counter_strike_2/egg-counter--strike2.yaml",
		// The files are about 35 GB and grow with updates. The second port is SourceTV.
		MemoryMB: 4 << 10,
		DiskMB:   50 << 10,
		Ports:    2,
	},
	{
		ID: "enshrouded", Name: "Enshrouded", Game: "Enshrouded",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "enshrouded/egg-enshrouded.json",
		// The studio asks for 12 GB and more for a full server, and the Windows server
		// runs under Proton. The second port is the query port.
		MemoryMB: 12 << 10,
		DiskMB:   10 << 10,
		Ports:    2,
	},
	{
		ID: "garrys-mod", Name: "Garry's Mod", Game: "Garry's Mod",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "gmod/egg-garrys-mod.yaml",
		// The server is about 4 GB; workshop content and mounted games add to it.
		MemoryMB: 2 << 10,
		DiskMB:   10 << 10,
	},
	{
		ID: "palworld", Name: "Palworld", Game: "Palworld",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "palworld/egg-palworld.yaml",
	},
	{
		ID: "project-zomboid", Name: "Project Zomboid", Game: "Project Zomboid",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "project_zomboid/egg-project-zomboid.json",
		// It runs on a JVM that grows with the map. The second port is Steam's.
		MemoryMB: 4 << 10,
		DiskMB:   10 << 10,
		Ports:    2,
	},
	{
		ID: "rust", Name: "Rust", Game: "Rust",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "rust/vanilla/egg-rust.yaml",
		// The game files alone are about 10 GB, and the server holds the
		// whole map in memory. It listens on four ports: the game's, the
		// query, RCON and the companion app.
		MemoryMB: 8 << 10,
		DiskMB:   30 << 10,
		Ports:    4,
	},
	{
		ID: "satisfactory", Name: "Satisfactory", Game: "Satisfactory",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "satisfactory/egg-satisfactory.json",
		// The studio asks for 8 GB, or 12 GB for late-game saves, and 25 GB of disk.
		// The second port is the reliable port.
		MemoryMB: 12 << 10,
		DiskMB:   25 << 10,
		Ports:    2,
	},
	{
		ID: "sons-of-the-forest", Name: "Sons of the Forest", Game: "Sons of the Forest",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "sonsoftheforest/egg-sons-of-the-forest.json",
		// The Windows server runs under Wine. The other two ports are query and
		// blob sync.
		MemoryMB: 8 << 10,
		DiskMB:   10 << 10,
		Ports:    3,
	},
	{
		ID: "squad", Name: "Squad", Game: "Squad",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "squad/egg-squad.json",
		// A 100-player match needs 8 GB, and the files are about 18 GB. The other two
		// ports are query and beacon.
		MemoryMB: 8 << 10,
		DiskMB:   25 << 10,
		Ports:    3,
	},
	{
		ID: "v-rising", Name: "V Rising", Game: "V Rising",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "v_rising/v_rising_vanilla/egg-v-rising.json",
		// The Windows server runs under Wine. The second port is query. RCON is off by
		// unless the owner turns it on, so its port is not opened.
		MemoryMB: 8 << 10,
		DiskMB:   10 << 10,
		Ports:    2,
	},
	{
		ID: "valheim", Name: "Valheim", Game: "Valheim",
		Repo:   "pelican-eggs/games-steamcmd",
		Commit: steamCommit,
		Path:   "valheim/valheim_vanilla/egg-valheim.json",
		// The game's own minimum is 4 GB. Steam queries the port after the game's
		// and crossplay uses the one after that; the egg has a variable for neither.
		MemoryMB: 4 << 10,
		DiskMB:   5 << 10,
		Ports:    3,
		Block:    3,
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

// The commits of the repositories the games come from.
const (
	minecraftCommit = "75bf05db3c6c305e0fa6eef1d38c7e7176121de9"
	steamCommit     = "e17e2c3db36aaf1ddecbc227803dd6cdbb0e6b1f"
)

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
