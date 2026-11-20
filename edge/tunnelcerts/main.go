// Command tunnelcerts creates a small private certificate authority and the
// two certificates the tunnel needs:
//
//	ca.pem, ca-key.pem         the authority (keep ca-key.pem private, off the VPS)
//	edge.pem, edge-key.pem     edge's server certificate for "tunnel.internal" → VPS
//	agent.pem, agent-key.pem   the agent's client certificate                  → laptop
//
// The tunnel trusts only this CA, so a certificate from any public authority
// (or anyone else's CA) can't impersonate either side.
//
//	go run ./tunnelcerts -out ~/.config/ledger/tunnel-pki
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	out := flag.String("out", "tunnel-pki", "folder to write the files to")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if _, err := os.Stat(filepath.Join(out, "ca-key.pem")); err == nil {
		return fmt.Errorf("%s already has a CA; not overwriting it", out)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	now := time.Now()

	caKey, caCert, err := issue(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "edge tunnel CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil, nil)
	if err != nil {
		return err
	}
	if err := save(out, "ca", caKey, caCert); err != nil {
		return err
	}

	for _, c := range []struct {
		name string
		tmpl *x509.Certificate
	}{
		{"edge", &x509.Certificate{
			Subject:     pkix.Name{CommonName: "tunnel.internal"},
			DNSNames:    []string{"tunnel.internal"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}},
		{"agent", &x509.Certificate{
			Subject:     pkix.Name{CommonName: "agent"},
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}},
	} {
		c.tmpl.NotBefore = now.Add(-time.Hour)
		c.tmpl.NotAfter = now.AddDate(5, 0, 0)
		c.tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		key, cert, err := issue(c.tmpl, caCert, caKey)
		if err != nil {
			return err
		}
		if err := save(out, c.name, key, cert); err != nil {
			return err
		}
	}
	fmt.Println("wrote ca, edge and agent certificates to", out)
	return nil
}

// issue creates a key and a certificate signed by parent (self-signed if nil).
func issue(tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = serial
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, err
}

func save(dir, name string, key *ecdsa.PrivateKey, cert *x509.Certificate) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-key.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}
