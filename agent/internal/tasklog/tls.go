package tasklog

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Reading output from another machine goes over TLS, to a listener the host's owner turned on.
// It is the one place a host accepts a connection, so it serves one thing: the output of a
// task, to whoever shows the secret whose hash came with that task.
//
// The listener has a key of its own, not the host's identity: that one is Ed25519, which many
// TLS clients will not take, and a key that only ever signs hello messages should not also
// sit behind an open port. There is no certificate authority. A caller is told the key's pin,
// the SHA-256 of its public key, and connects only to a server that has that key.

// LoadOrCreateKey reads the listener's P-256 key from path, or makes one there.
func LoadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s does not hold a key", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, os.Rename(tmp, path)
}

// Pin identifies a public key: the SHA-256 of its SubjectPublicKeyInfo, in base64. It is the
// value curl takes as --pinnedpubkey "sha256//<pin>".
func Pin(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// Certificate makes a self-signed certificate for the key. Callers trust the key by its pin,
// so the certificate says nothing they rely on; it is made anew each time the agent starts.
func Certificate(key *ecdsa.PrivateKey, address string) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mcc task output"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if host, _, err := net.SplitHostPort(address); err == nil && host != "" {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
		} else {
			tmpl.DNSNames = []string{host}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ServeTLS serves the store on ln until ctx ends. Unlike the owner's socket it lets no one in
// without a task's secret.
func (s *Store) ServeTLS(ctx context.Context, ln net.Listener, cert tls.Certificate) error {
	srv := &http.Server{
		Handler:           s.Handler(false),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		// Handshake failures from port scanners are not the owner's problem to read.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	tl := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err := srv.Serve(tl); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ErrWrongKey is returned when the server at the address does not have the pinned key.
var ErrWrongKey = errors.New("the server does not have the pinned key; nothing was sent to it")

// ReadTLS copies a task's output from the listener of the host that ran it to w. The secret is
// sent only once the server has shown that it holds the key with this pin.
func ReadTLS(ctx context.Context, address, pin, id, secret string, offset int64, follow bool, w io.Writer) (End, error) {
	pin = strings.TrimPrefix(pin, "sha256//")
	want, err := base64.StdEncoding.DecodeString(pin)
	if err != nil || len(want) != sha256.Size {
		return End{}, fmt.Errorf("the pin must be a SHA-256 in base64")
	}
	if secret == "" {
		return End{}, fmt.Errorf("reading from another machine needs the task's secret")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// There is no authority to verify against: the pin is the whole of the trust, and it is
			// checked here, before the handshake completes and so before any request is written.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return ErrWrongKey
				}
				got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
				if subtle.ConstantTimeCompare(got[:], want) != 1 {
					return ErrWrongKey
				}
				return nil
			},
		},
		// A proxy would see the address, and is not what the caller pinned.
		Proxy: nil,
	}}
	defer client.CloseIdleConnections()
	end, err := Read(ctx, client, "https://"+address, id, secret, offset, follow, w)
	if err != nil && errors.Is(err, ErrWrongKey) {
		return end, ErrWrongKey
	}
	return end, err
}
