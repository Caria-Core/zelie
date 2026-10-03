package core

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"sync"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/msg"
)

// The SFTP port and the game forwards must never share a port: a forward
// would send SFTP logins to a game server, which could read the passwords.
// The panel checks this too, but the core is the one that opens both, so it
// refuses as well. One lock covers the check and the change, so the two
// requests cannot pass each other.

var (
	errForwardOnSFTP = msg.Define(http.StatusConflict, "forward.sftp_port", "Port {port} is the SFTP port.")
	errSFTPOnForward = msg.Define(http.StatusConflict, "sftp.port_forwarded", "Port {port} is open to a game server.")

	sftpGuard sync.Mutex
)

// forwardLister is what the engine offers to learn which ports are
// forwarded.
type forwardLister interface {
	ForwardedPorts() ([]uint16, error)
}

var _ forwardLister = (*engine.Engine)(nil)

var listenStream = regexp.MustCompile(`(?m)^ListenStream=(\d+)\s*$`)

// Port is the port the socket listens on: the one in the drop-in, or the
// unit's own when the panel never changed it.
func (c *SFTPSocket) Port() (int, error) {
	b, err := os.ReadFile(c.DropIn)
	if errors.Is(err, os.ErrNotExist) {
		return install.DefaultSFTPPort, nil
	}
	if err != nil {
		return 0, err
	}
	m := listenStream.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return install.DefaultSFTPPort, nil
	}
	return strconv.Atoi(string(m[len(m)-1][1]))
}

// guardForwards refuses forwards on the SFTP port. On success the caller
// holds the lock until it calls the returned function.
func (s *Server) guardForwards(w http.ResponseWriter, forwards []engine.Forward) (unlock func(), ok bool) {
	sftpGuard.Lock()
	if s.SFTPSocket != nil && len(forwards) > 0 {
		port, err := s.SFTPSocket.Port()
		if err != nil {
			sftpGuard.Unlock()
			s.Log.Error("read the SFTP port", "err", err)
			writeError(w, http.StatusInternalServerError, errors.New("could not read the SFTP port"))
			return nil, false
		}
		if slices.ContainsFunc(forwards, func(f engine.Forward) bool { return int(f.Port) == port }) {
			sftpGuard.Unlock()
			writeError(w, http.StatusConflict, errForwardOnSFTP.Err("port", port))
			return nil, false
		}
	}
	return sftpGuard.Unlock, true
}

// guardSFTPPort refuses a port that a game server has open. On success the
// caller holds the lock until it calls the returned function.
func (s *Server) guardSFTPPort(w http.ResponseWriter, port int) (unlock func(), ok bool) {
	sftpGuard.Lock()
	l, isLister := s.Engine.(forwardLister)
	if !isLister {
		return sftpGuard.Unlock, true
	}
	ports, err := l.ForwardedPorts()
	if err != nil {
		sftpGuard.Unlock()
		s.Log.Error("read the forwards", "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("could not read the forwards"))
		return nil, false
	}
	if port > 0 && port <= 65535 && slices.Contains(ports, uint16(port)) {
		sftpGuard.Unlock()
		writeError(w, http.StatusConflict, errSFTPOnForward.Err("port", port))
		return nil, false
	}
	return sftpGuard.Unlock, true
}
