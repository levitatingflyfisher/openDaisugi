package web

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PaneRefusal is the message a pane gets when it tries to answer an ask.
// It is the same line the coppice server sends.
const PaneRefusal = "a pane can propose. It cannot allow."

// isLocalIP reports whether ip is a loopback address or one of this host's
// own addresses, so a connection from it comes from a process on this box.
func isLocalIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		// With no list of our own addresses, every peer might be local.
		return true
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// parseProcAddr reads one address from /proc/net/tcp or tcp6: the IP in hex,
// each 32-bit word in host order, a colon, and the port in hex.
func parseProcAddr(s string) (net.IP, int, error) {
	host, port, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, fmt.Errorf("no port in %q", s)
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, fmt.Errorf("cannot read the address %q", s)
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(ip[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot read the port in %q", s)
	}
	return ip, int(p), nil
}

// peerHello is the part of a hello that names the local process a request
// came from. It is nil for a peer on another host, and nil when every
// process that holds the peer's socket is this web server itself: a web
// server that runs inside a pane already has a pane connection upstream.
// A local peer that cannot be placed is peer_unknown, which the coppice
// server refuses the allow verbs.
func peerHello(r *http.Request) map[string]any {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	remote, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
	if err != nil || local == nil {
		return map[string]any{"peer_unknown": true}
	}
	pids, isLocal, err := localPeerPIDs(remote, local)
	if !isLocal {
		return nil
	}
	if err != nil || len(pids) == 0 {
		return map[string]any{"peer_unknown": true}
	}
	others := pids[:0:0]
	for _, p := range pids {
		if p != os.Getpid() {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		return nil
	}
	return map[string]any{"peer_pids": others}
}

// placeAllows asks the coppice server whether the peer a hello names may
// answer an ask. Any failure is a no.
func placeAllows(ctx context.Context, d Dialer, hello map[string]any) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := map[string]any{"cmd": "hello"}
	for k, v := range hello {
		req[k] = v
	}
	msg, err := Call(ctx, d, req)
	if err != nil {
		return false
	}
	res, _ := msg["result"].(map[string]any)
	allow, _ := res["allow"].(bool)
	return allow
}

// errNotPlaced means the peer is on this box but no process holds its
// socket, or /proc could not be read.
var errNotPlaced = errors.New("cannot find the local process behind this connection")

// helloLine is the hello a websocket sends upstream first, with the id
// the forwarding loop drops so the browser never sees its reply.
func helloLine(hello map[string]any) ([]byte, error) {
	req := map[string]any{"id": webHelloID, "cmd": "hello"}
	for k, v := range hello {
		req[k] = v
	}
	return json.Marshal(req)
}

// webHelloID is the id of the hello a websocket sends upstream.
const webHelloID = "web-hello"

// isWebHelloReply reports whether a line from the server answers the hello
// a websocket sent for its client. The browser never asked for it.
func isWebHelloReply(line []byte) bool {
	if !strings.Contains(string(line), webHelloID) {
		return false
	}
	var m struct {
		ID string `json:"id"`
	}
	return json.Unmarshal(line, &m) == nil && m.ID == webHelloID
}
