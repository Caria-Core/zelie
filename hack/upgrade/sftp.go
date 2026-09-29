package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpCheck logs in to the game server over SFTP, as a person's program
// would: with the server's own password, which the panel shows once, to the
// port the panel names. A server updated from a release without SFTP gets
// its user and service from the new core, so the port may need a moment.
func sftpCheck(c *client) error {
	step("make an SFTP password for the server")
	if err := c.confirm(); err != nil {
		return err
	}
	var made struct {
		Password string `json:"password"`
	}
	if err := c.do("POST", "/api/games/"+gameID+"/sftp/password", nil, &made); err != nil {
		return err
	}

	step("log in over SFTP with it")
	var info struct {
		Host    string `json:"host"`
		Port    int    `json:"port"`
		User    string `json:"user"`
		HostKey string `json:"host_key"`
	}
	var conn *ssh.Client
	// The service reports its host key to the panel with each poll, which
	// is how the panel can show it.
	err := waitFor("the SFTP server to answer with a host key", 3*time.Minute, func() error {
		if err := c.do("GET", "/api/games/"+gameID+"/sftp", nil, &info); err != nil {
			return err
		}
		if info.HostKey == "" {
			return errors.New("the panel has not been told a host key yet")
		}
		var err error
		conn, err = ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(info.Port)), &ssh.ClientConfig{
			User:    info.User,
			Auth:    []ssh.AuthMethod{ssh.Password(made.Password)},
			Timeout: 10 * time.Second,
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				if got := ssh.FingerprintSHA256(key); got != info.HostKey {
					return stop{fmt.Errorf("the host key is %s, the panel says %s", got, info.HostKey)}
				}
				return nil
			},
		})
		return err
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Println("host key", info.HostKey)

	step("refuse a wrong password")
	if bad, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(info.Port)), &ssh.ClientConfig{
		User: info.User, Auth: []ssh.AuthMethod{ssh.Password("not the password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
	}); err == nil {
		bad.Close()
		return errors.New("a wrong SFTP password was accepted")
	}

	step("list the server's files and download server.properties")
	fx, err := sftp.NewClient(conn)
	if err != nil {
		return err
	}
	defer fx.Close()
	infos, err := fx.ReadDir("/")
	if err != nil {
		return err
	}
	found := false
	for _, fi := range infos {
		found = found || fi.Name() == "server.properties"
	}
	if !found {
		return fmt.Errorf("server.properties is not in the listing of %d entries", len(infos))
	}
	f, err := fx.Open("/server.properties")
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), "server-port") {
		return fmt.Errorf("server.properties reads %q", b)
	}
	fmt.Printf("server.properties: %d bytes\n", len(b))
	return sftpWrite(c, fx)
}

// sftpWrite uploads, renames and removes a file, as a person moving a plugin
// in would, and checks the panel sees what SFTP wrote, owned by the server's
// user, and that no path leads out of the server's files.
func sftpWrite(c *client, fx *sftp.Client) error {
	step("upload, rename and remove a file over SFTP")
	const text = "written over SFTP\n"
	if err := fx.Mkdir("/sftp-check"); err != nil {
		return err
	}
	f, err := fx.Create("/sftp-check/upload.tmp")
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(text)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := fx.Rename("/sftp-check/upload.tmp", "/sftp-check/hello.txt"); err != nil {
		return err
	}
	// SFTP does not say who owns a file, so look on the disk.
	disk, err := filepath.Glob("/var/lib/zelie/volumes/*/sftp-check/hello.txt")
	if err != nil || len(disk) != 1 {
		return fmt.Errorf("the uploaded file on the disk: %v %v", disk, err)
	}
	fi, err := os.Stat(disk[0])
	if err != nil {
		return err
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 988 || st.Gid != 988 {
		return fmt.Errorf("the uploaded file is owned by %d:%d, want 988:988", st.Uid, st.Gid)
	}
	got, err := c.send("GET", "/api/games/"+gameID+"/files/content?path=sftp-check/hello.txt", nil)
	if err != nil {
		return err
	}
	if string(got) != text {
		return fmt.Errorf("the panel reads the uploaded file as %q", got)
	}

	step("stay inside the server's files")
	for _, p := range []string{"/../../../../etc/passwd", "../../../../etc/passwd", "/sftp-check/../../../etc/passwd"} {
		if f, err := fx.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if strings.Contains(string(b), "root:") {
				return fmt.Errorf("%s reached the machine's /etc/passwd", p)
			}
		}
	}

	if err := fx.Remove("/sftp-check/hello.txt"); err != nil {
		return err
	}
	if err := fx.RemoveDirectory("/sftp-check"); err != nil {
		return err
	}
	if _, err := fx.Stat("/sftp-check"); err == nil {
		return errors.New("the removed folder is still there")
	}
	return nil
}
