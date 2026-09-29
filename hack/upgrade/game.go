package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const gameID = "paper"

// gameStatus is the part of a game server's description this check reads.
type gameStatus struct {
	State   string `json:"state"`
	Install struct {
		State      string `json:"state"`
		Deployment int64  `json:"deployment"`
	} `json:"install"`
	Ports []struct {
		Port    int  `json:"port"`
		Default bool `json:"default"`
	} `json:"ports"`
	Crashing json.RawMessage `json:"crashing"`
}

// game runs a Paper server on the updated panel from install to a clean
// stop, and joins it the way a player's launcher does.
func game(c *client, stateFile string) error {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return err
	}
	st := &state{}
	if err := json.Unmarshal(b, st); err != nil {
		return err
	}
	c.st = st

	step("log in")
	if err := c.login(); err != nil {
		return err
	}

	step("open a block of ports on this machine")
	var r struct {
		IP    string `json:"ip"`
		Ports string `json:"ports"`
	}
	if err := c.do("GET", "/api/nodes/1/allocations/suggest", nil, &r); err != nil {
		return err
	}
	if err := c.do("POST", "/api/nodes/1/allocations", r, nil); err != nil {
		return err
	}

	step("create a Paper server")
	req := map[string]any{"name": gameID, "egg": "minecraft-paper", "memory_mb": 2048, "cpus": 2, "ports": 1, "accept_eula": true}
	if err := c.do("POST", "/api/games", req, nil); err != nil {
		return err
	}
	if err := c.waitInstalled(); err != nil {
		return err
	}

	step("start it")
	var started struct {
		Deployment int64 `json:"deployment"`
	}
	if err := c.do("POST", "/api/games/"+gameID+"/power", map[string]string{"action": "start"}, &started); err != nil {
		return err
	}
	err = waitFor("the server to run", 5*time.Minute, func() error {
		g, err := c.gameStatus()
		if err != nil {
			return err
		}
		switch g.State {
		case "running":
			return nil
		case "crashed", "stopped":
			return stop{fmt.Errorf("the server is %s", g.State)}
		}
		return errors.New(g.State)
	})
	if err != nil {
		c.printLog(started.Deployment)
		return err
	}

	step("read the console and run a command")
	if err := c.console(); err != nil {
		return err
	}

	step("ping it from this machine's own address, as a player would")
	g, err := c.gameStatus()
	if err != nil {
		return err
	}
	port := 0
	for _, p := range g.Ports {
		if p.Default {
			port = p.Port
		}
	}
	if port == 0 {
		return fmt.Errorf("the server has no default port: %+v", g.Ports)
	}
	ip, err := ownAddress()
	if err != nil {
		return err
	}
	var status string
	if err := waitFor("the server to answer a ping", time.Minute, func() error {
		var err error
		status, err = serverListPing(net.JoinHostPort(ip, fmt.Sprint(port)))
		return err
	}); err != nil {
		return err
	}
	if !strings.Contains(status, `"version"`) {
		return fmt.Errorf("the status has no version: %s", status)
	}
	fmt.Println(status)

	step("stop it cleanly")
	if err := c.do("POST", "/api/games/"+gameID+"/power", map[string]string{"action": "stop"}, nil); err != nil {
		return err
	}
	// Zelie kills a server that has not stopped after a minute; a stop
	// from the console takes a few seconds.
	if err := waitFor("the server to stop", 55*time.Second, func() error {
		g, err := c.gameStatus()
		if err != nil {
			return err
		}
		if g.State != "stopped" {
			return errors.New(g.State)
		}
		return nil
	}); err != nil {
		return err
	}
	step("the stop was not a crash")
	// The supervisor looks every few seconds, and would bring back a server
	// it took for crashed.
	time.Sleep(15 * time.Second)
	if g, err = c.gameStatus(); err != nil {
		return err
	}
	if g.State != "stopped" || len(g.Crashing) > 0 && string(g.Crashing) != "null" {
		return fmt.Errorf("after the stop the server is %s, crashing %s", g.State, g.Crashing)
	}
	var m struct {
		Crashes int `json:"crashes"`
	}
	if err := c.do("GET", "/api/apps/"+gameID+"/metrics", nil, &m); err != nil {
		return err
	}
	if m.Crashes != 0 {
		return fmt.Errorf("Zelie brought the server back %d times", m.Crashes)
	}
	return nil
}

func (c *client) gameStatus() (gameStatus, error) {
	var g gameStatus
	err := c.do("GET", "/api/games/"+gameID, nil, &g)
	return g, err
}

// waitInstalled waits for the install and, if it fails, prints its log.
func (c *client) waitInstalled() error {
	var id int64
	err := waitFor("the install", 10*time.Minute, func() error {
		g, err := c.gameStatus()
		if err != nil {
			return err
		}
		id = g.Install.Deployment
		switch g.Install.State {
		case "installed":
			return nil
		case "failed":
			return stop{errors.New("the install failed")}
		}
		return errors.New(g.Install.State)
	})
	if err != nil {
		c.printLog(id)
	}
	return err
}

// printLog prints a deployment's log, for a failure to be read from.
func (c *client) printLog(id int64) {
	if id == 0 {
		return
	}
	b, err := c.send("GET", fmt.Sprintf("/api/apps/%s/deployments/%d/log", gameID, id), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deployment log:", err)
		return
	}
	fmt.Fprintf(os.Stderr, "-- log of deployment %d\n%s\n-- end of log\n", id, b)
}

type consoleMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// console opens the server's console as the panel's page does: the socket
// comes with a token, and its Origin is the panel's own address.
func (c *client) console() error {
	var t struct {
		Token string `json:"token"`
	}
	if err := c.do("POST", "/api/games/"+gameID+"/console/token", nil, &t); err != nil {
		return err
	}
	addr := strings.TrimPrefix(c.base, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cookie := ""
	for k, v := range c.cookies {
		cookie += k + "=" + v + "; "
	}
	conn, _, err := websocket.Dial(ctx, "ws://"+panelHost+"/api/games/"+gameID+"/console?token="+t.Token, &websocket.DialOptions{
		// The proxy is reached on its tunnel port, and the panel takes
		// the scheme of the request as plain HTTP.
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}},
		HTTPHeader: http.Header{
			"Origin": {"http://" + panelHost},
			"Cookie": {cookie},
		},
	})
	if err != nil {
		return fmt.Errorf("open the console: %w", err)
	}
	defer conn.CloseNow()

	// The backlog holds the line the game printed when it finished starting.
	if err := readUntil(ctx, conn, "Done (", 30*time.Second); err != nil {
		return err
	}
	cmd, _ := json.Marshal(map[string]string{"type": "command", "data": "list"})
	if err := conn.Write(ctx, websocket.MessageText, cmd); err != nil {
		return err
	}
	if err := readUntil(ctx, conn, "players online", 30*time.Second); err != nil {
		return err
	}
	return conn.Close(websocket.StatusNormalClosure, "")
}

// readUntil reads console lines until one holds want.
func readUntil(ctx context.Context, conn *websocket.Conn, want string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("waiting for %q in the console: %w", want, err)
		}
		var m consoleMessage
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.Type {
		case "line":
			if strings.Contains(m.Data, want) {
				return nil
			}
		case "error":
			return fmt.Errorf("the console answered %s: %s", m.Code, m.Message)
		}
	}
}

// ownAddress is the first address of this machine's own that is not
// loopback. A packet to it enters through the network stack and meets the
// port forwards, which one to 127.0.0.1 may not.
func ownAddress() (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifs {
		// Container networks are behind the forwards, not in front of them.
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 ||
			strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") ||
			strings.HasPrefix(ifc.Name, "veth") || strings.HasPrefix(ifc.Name, "zelie") {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsGlobalUnicast() {
				return n.IP.String(), nil
			}
		}
	}
	return "", errors.New("this machine has no IPv4 address besides loopback")
}

// serverListPing asks a Minecraft server for its status, as the game's
// server list does, and returns the JSON it answers with.
func serverListPing(addr string) (string, error) {
	host, portStr, _ := net.SplitHostPort(addr)
	var port uint16
	fmt.Sscan(portStr, &port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Every packet is its length and its body, both as VarInts where a
	// number is. Handshake: id 0, protocol version, address, port, and
	// next state 1 for a status. Then the status request: id 0.
	hs := varInt(0)
	hs = append(hs, varInt(767)...)
	hs = append(hs, varInt(len(host))...)
	hs = append(hs, host...)
	hs = binary.BigEndian.AppendUint16(hs, port)
	hs = append(hs, varInt(1)...)
	out := append(varInt(len(hs)), hs...)
	out = append(out, 1, 0)
	if _, err := conn.Write(out); err != nil {
		return "", err
	}

	r := bufio.NewReader(conn)
	if _, err := readVarInt(r); err != nil { // packet length
		return "", err
	}
	if id, err := readVarInt(r); err != nil || id != 0 {
		return "", fmt.Errorf("the status answer has id %d: %v", id, err)
	}
	n, err := readVarInt(r)
	if err != nil || n <= 0 || n > 1<<20 {
		return "", fmt.Errorf("the status answer has length %d: %v", n, err)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func varInt(n int) []byte {
	var b []byte
	for u := uint32(n); ; u >>= 7 {
		if u < 0x80 {
			return append(b, byte(u))
		}
		b = append(b, byte(u)|0x80)
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var n uint32
	for shift := 0; shift < 35; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		n |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return int(int32(n)), nil
		}
	}
	return 0, errors.New("VarInt is too long")
}
