// Command edge routes TLS connections on :443 by hostname (SNI) without
// decrypting them. It never holds a certificate for the services it routes:
// the backend does.
//
// It is also the VPS end of the tunnel: the agent on the laptop connects to
// "tunnel.internal" (the one name edge terminates TLS for, with mutual TLS),
// and edge sends "tunnel:" routes through that connection as streams.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackdzi/ledger-proxy/edge/mux"
)

// routes: hostname → backend. "tunnel:NAME" means the service NAME on the
// far side of the tunnel. Anything not listed goes to fallback.
var routes = map[string]string{
	"ledger.techsummary.online": "tunnel:ledger", // → agent → ledgerd on the laptop
}

const (
	fallback     = "127.0.0.1:8443" // nginx, serving the website (a port SELinux allows nginx to use)
	sshTunnel    = "127.0.0.1:9443" // the old SSH tunnel: used while the agent isn't connected
	tunnelHost   = "tunnel.internal"
	maxConns     = 512             // total connections at once
	maxPerIP     = 20              // connections at once from one IP
	helloTimeout = 5 * time.Second // a client must send its ClientHello this fast
)

var (
	mu    sync.Mutex
	perIP = map[string]int{}
	slots = make(chan struct{}, maxConns)

	tunnelTLS *tls.Config                 // nil: tunnel disabled
	tunnel    atomic.Pointer[mux.Session] // the connected agent, if any
)

func main() {
	addr := ":443"
	if a := os.Getenv("LISTEN_ADDR"); a != "" {
		addr = a
	}
	if dir := os.Getenv("EDGE_TUNNEL_DIR"); dir != "" {
		cfg, err := loadTunnelTLS(dir)
		if err != nil {
			slog.Error("tunnel certificates", "err", err)
			os.Exit(1)
		}
		tunnelTLS = cfg
		slog.Info("tunnel enabled", "host", tunnelHost)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	slog.Info("edge listening", "addr", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			slog.Error("accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handle(conn)
	}
}

func handle(client net.Conn) {
	defer client.Close()
	ip, _, _ := net.SplitHostPort(client.RemoteAddr().String())

	// Limits: drop the connection if we're full or this IP has too many open.
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return
	}
	if !acquire(ip) {
		return
	}
	defer release(ip)

	// Read just the ClientHello (unencrypted) to learn the hostname.
	client.SetReadDeadline(time.Now().Add(helloTimeout))
	hello, host, err := readClientHello(client)
	if err != nil {
		return // not TLS, or too slow
	}
	client.SetReadDeadline(time.Time{})

	host = strings.ToLower(host)
	if host == tunnelHost {
		if tunnelTLS != nil {
			serveTunnel(client, hello, ip)
		}
		return
	}

	backend, known := routes[host]
	if !known {
		backend = fallback
	}
	var up io.ReadWriteCloser
	if service, ok := strings.CutPrefix(backend, "tunnel:"); ok {
		if sess := tunnel.Load(); sess != nil {
			if st, err := sess.Open(service); err == nil {
				up = st
			}
		}
		if up == nil {
			backend = sshTunnel // agent not connected: use the old SSH tunnel
		}
	}
	if up == nil {
		c, err := net.DialTimeout("tcp", backend, 5*time.Second)
		if err != nil {
			slog.Warn("backend unreachable", "host", host, "backend", backend)
			return
		}
		up = c
	}
	defer up.Close()

	// nginx gets a PROXY protocol line first, so it still sees the real
	// client IP (its rate limits depend on it). ledgerd doesn't need it.
	if !known {
		if err := writeProxyHeader(up, client); err != nil {
			return
		}
	}

	// Replay the ClientHello, then copy bytes both ways until one side closes.
	if _, err := up.Write(hello); err != nil {
		return
	}
	go func() {
		io.Copy(up, client)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() // tell the backend the client is done sending
		}
	}()
	io.Copy(client, up)
}

// serveTunnel accepts the agent: edge terminates TLS for tunnelHost and
// requires a client certificate signed by our own CA. The newest agent
// connection wins (e.g. after the laptop switched networks).
func serveTunnel(client net.Conn, hello []byte, ip string) {
	replay := &prefixConn{Conn: client, r: io.MultiReader(bytes.NewReader(hello), client)}
	tc := tls.Server(replay, tunnelTLS)
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		slog.Warn("tunnel handshake failed", "from", ip, "err", err)
		return
	}
	tc.SetDeadline(time.Time{})

	sess := mux.New(tc)
	if old := tunnel.Swap(sess); old != nil {
		old.Close()
	}
	slog.Info("tunnel agent connected", "from", ip)
	<-sess.Done()
	tunnel.CompareAndSwap(sess, nil)
	slog.Warn("tunnel agent disconnected", "from", ip, "err", sess.Err())
}

// prefixConn replays bytes already read (the ClientHello) before reading
// the rest of the connection.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func loadTunnelTLS(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "edge.pem"), filepath.Join(dir, "edge-key.pem"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.pem: no certificate")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // only our agent may connect
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// readClientHello reads the client's first TLS message and returns its raw
// bytes plus the requested hostname. It lets Go's TLS code parse the
// message, then aborts the handshake before anything is sent back.
func readClientHello(c net.Conn) ([]byte, string, error) {
	var buf bytes.Buffer
	var host string
	errDone := errors.New("got hello")
	err := tls.Server(readOnly{io.TeeReader(c, &buf)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			host = h.ServerName
			return nil, errDone
		},
	}).Handshake()
	if !errors.Is(err, errDone) || host == "" {
		return nil, "", fmt.Errorf("no ClientHello with SNI: %v", err)
	}
	return buf.Bytes(), host, nil
}

// readOnly is a net.Conn that can only be read from: the TLS parser above
// must never send anything to the client.
type readOnly struct{ r io.Reader }

func (c readOnly) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnly) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (readOnly) Close() error                     { return nil }
func (readOnly) LocalAddr() net.Addr              { return nil }
func (readOnly) RemoteAddr() net.Addr             { return nil }
func (readOnly) SetDeadline(time.Time) error      { return nil }
func (readOnly) SetReadDeadline(time.Time) error  { return nil }
func (readOnly) SetWriteDeadline(time.Time) error { return nil }

// writeProxyHeader sends "PROXY TCP4 <client ip> <server ip> <ports>".
func writeProxyHeader(up io.Writer, client net.Conn) error {
	src := client.RemoteAddr().(*net.TCPAddr)
	dst := client.LocalAddr().(*net.TCPAddr)
	proto := "TCP4"
	if src.IP.To4() == nil {
		proto = "TCP6"
	}
	_, err := fmt.Fprintf(up, "PROXY %s %s %s %d %d\r\n", proto, src.IP, dst.IP, src.Port, dst.Port)
	return err
}

func acquire(ip string) bool {
	mu.Lock()
	defer mu.Unlock()
	if perIP[ip] >= maxPerIP {
		return false
	}
	perIP[ip]++
	return true
}

func release(ip string) {
	mu.Lock()
	defer mu.Unlock()
	if perIP[ip]--; perIP[ip] == 0 {
		delete(perIP, ip) // keep the map from growing forever
	}
}
