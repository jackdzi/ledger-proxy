// Command agent is the laptop side of the tunnel. It dials OUT to edge on the
// VPS (so it works behind campus NAT), proves who it is with a client
// certificate (mutual TLS), and serves every stream edge opens by connecting
// to the matching local service.
//
// Settings (environment):
//
//	TUNNEL_SERVER  edge's address, e.g. 203.0.113.10:443
//	TUNNEL_DIR     folder with ca.pem, agent.pem, agent-key.pem
//	SERVICES       name=address pairs, e.g. "ledger=127.0.0.1:8443"
package main

import (
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
	"time"

	"github.com/jackdzi/ledger-proxy/edge/mux"
)

func main() {
	server := os.Getenv("TUNNEL_SERVER")
	dir := os.Getenv("TUNNEL_DIR")
	services, err := parseServices(os.Getenv("SERVICES"))
	if server == "" || dir == "" || err != nil {
		slog.Error("set TUNNEL_SERVER, TUNNEL_DIR and SERVICES", "err", err)
		os.Exit(1)
	}
	cfg, err := tlsConfig(dir)
	if err != nil {
		slog.Error("certificates", "err", err)
		os.Exit(1)
	}

	// Reconnect forever, backing off 1 s, 2 s, 4 s … up to 30 s.
	backoff := time.Second
	for {
		start := time.Now()
		err := connect(server, cfg, services)
		if time.Since(start) > time.Minute {
			backoff = time.Second // it was up for a while: retry quickly
		}
		slog.Warn("tunnel down, reconnecting", "err", err, "in", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func tlsConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "agent.pem"), filepath.Join(dir, "agent-key.pem"))
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
		ServerName:   "tunnel.internal", // edge recognizes the tunnel by this name
		RootCAs:      pool,              // trust ONLY our own CA for the tunnel
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func connect(server string, cfg *tls.Config, services map[string]string) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", server, cfg)
	if err != nil {
		return err
	}
	sess := mux.New(conn)
	defer sess.Close()
	slog.Info("tunnel up", "server", server)
	for {
		st, err := sess.Accept()
		if err != nil {
			return err
		}
		go serve(st, services)
	}
}

// serve connects one stream to its local service and copies both ways.
func serve(st *mux.Stream, services map[string]string) {
	defer st.Close()
	addr, ok := services[st.Service()]
	if !ok {
		slog.Warn("unknown service requested", "service", st.Service())
		return // Close sends RST
	}
	local, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		slog.Warn("local service unreachable", "service", st.Service(), "err", err)
		return
	}
	defer local.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(local, st) // edge → service
		local.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(st, local) // service → edge
	st.CloseWrite()
	<-done // wait for both directions before closing
}

func parseServices(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("SERVICES: bad entry %q (want name=host:port)", pair)
		}
		m[name] = addr
	}
	return m, nil
}
