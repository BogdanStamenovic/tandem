// Package control is the local unix-socket API between the `tandem` CLI and
// a running daemon: one JSON request line, one JSON response.
package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

const SocketName = "control.sock"

type Request struct {
	Cmd    string `json:"cmd"`
	IP     string `json:"ip,omitempty"`
	Port   uint16 `json:"port,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
}

type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Handler answers one request. uid is the caller's (SO_PEERCRED).
type Handler func(req Request, uid int) (any, error)

// Serve listens on dir/control.sock. The socket is world-connectable so
// `tandem status` works unprivileged; handlers check uid where it matters.
func Serve(dir string, h Handler) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, SocketName)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o666)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("control: accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go serveOne(c.(*net.UnixConn), h)
		}
	}()
	return ln, nil
}

func serveOne(c *net.UnixConn, h Handler) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	uid := peerUID(c)
	var req Request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	var resp Response
	data, err := h(req, uid)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.OK = true
		resp.Data, _ = json.Marshal(data)
	}
	_ = json.NewEncoder(c).Encode(resp)
}

// Call sends req to the daemon whose run dir is dir.
func Call(dir string, req Request) (*Response, error) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, SocketName), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
