package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

func TestForwardsAndTheSFTPPortStayApart(t *testing.T) {
	s, sock, _ := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	f := s.Engine.(*fakeEngine)
	forward := func(port string) *httptest.ResponseRecorder {
		return request(t, s, panel, "PUT", "/v1/forwards/mc", `{"forwards":[{"port":`+port+`,"proto":"tcp","target":1}]}`)
	}
	setSFTP := func(port string) *httptest.ResponseRecorder {
		return request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":`+port+`}`)
	}

	// Before the panel ever sets it, the unit's own port is the SFTP port.
	if rec := forward("2222"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"forward.sftp_port"`) {
		t.Errorf("a forward on the default SFTP port: %d %s", rec.Code, rec.Body)
	}
	if rec := setSFTP("2300"); rec.Code != http.StatusNoContent {
		t.Fatalf("set SFTP: %d %s", rec.Code, rec.Body)
	}
	if rec := forward("2300"); rec.Code != http.StatusConflict {
		t.Errorf("a forward on the set SFTP port: %d %s", rec.Code, rec.Body)
	}
	if rec := forward("2222"); rec.Code != http.StatusNoContent {
		t.Errorf("the old port is free now: %d %s", rec.Code, rec.Body)
	}
	if len(f.forwards["mc"]) != 1 {
		t.Errorf("forwards %+v", f.forwards)
	}

	rec := setSFTP("2222")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"sftp.port_forwarded"`) {
		t.Errorf("SFTP onto a forwarded port: %d %s", rec.Code, rec.Body)
	}
	if port, err := sock.Port(); err != nil || port != 2300 {
		t.Errorf("the socket moved to %d (%v)", port, err)
	}
}

func (f *fakeEngine) ForwardedPorts() ([]uint16, error) {
	var ports []uint16
	for _, list := range f.forwards {
		for _, x := range list {
			ports = append(ports, x.Port)
		}
	}
	return ports, nil
}
