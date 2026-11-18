// Package identity manages the host's Ed25519 key. The private key never leaves the host; the
// control plane stores only the public key, and revoking a host is deleting it.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const fileName = "identity.key"

type Identity struct {
	priv ed25519.PrivateKey
}

// Generate creates a new identity and writes it to dir, refusing to overwrite an existing one.
func Generate(dir string) (*Identity, error) {
	p := filepath.Join(dir, fileName)
	if _, err := os.Stat(p); err == nil {
		return nil, fmt.Errorf("%s already exists; this host is already enrolled (delete it to re-enroll)", p)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(priv.Seed())), 0o600); err != nil {
		return nil, err
	}
	return &Identity{priv: priv}, nil
}

func Load(dir string) (*Identity, error) {
	p := filepath.Join(dir, fileName)
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users; run chmod 600 on it", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity.key is corrupt")
	}
	return &Identity{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// FromSeed builds an identity in memory (tests).
func FromSeed(seed []byte) *Identity {
	return &Identity{priv: ed25519.NewKeyFromSeed(seed)}
}

func (i *Identity) PublicKey() string {
	return base64.StdEncoding.EncodeToString(i.priv.Public().(ed25519.PublicKey))
}

// SignHello signs the connection challenge, binding it to this host id.
func (i *Identity) SignHello(nonce, hostID string) string {
	msg := []byte("mcc-hello\n" + nonce + "\n" + hostID)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(i.priv, msg))
}
