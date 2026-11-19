package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Credentials are what signing in leaves on this machine: a token for one control plane.
// They are a person's, and are kept apart from a host's own identity.
type Credentials struct {
	Server    string    `json:"server"`
	Token     string    `json:"token"`
	User      string    `json:"user,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// CredentialsPath is where they are kept: MCC_CREDENTIALS, or mcc/credentials.json in the
// user's configuration directory.
func CredentialsPath() (string, error) {
	if p := os.Getenv("MCC_CREDENTIALS"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mcc", "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mcc", "credentials.json"), nil
}

// ErrSignedOut is returned when there is no token to act with.
var ErrSignedOut = errors.New("not signed in: run `mcc auth login`")

// LoadCredentials returns what to act with. A token in MCC_API_TOKEN wins over a saved
// sign-in, with MCC_SERVER_URL saying where it is for.
func LoadCredentials() (Credentials, error) {
	if t := os.Getenv("MCC_API_TOKEN"); t != "" {
		server := os.Getenv("MCC_SERVER_URL")
		if server == "" {
			server = DefaultServer
		}
		return Credentials{Server: server, Token: t}, nil
	}
	var c Credentials
	p, err := CredentialsPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, ErrSignedOut
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil || c.Token == "" || c.Server == "" {
		return c, ErrSignedOut
	}
	return c, nil
}

// SaveCredentials writes them where only this user can read them.
func SaveCredentials(c Credentials) error {
	p, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// DeleteCredentials forgets a saved sign-in. It is not an error if there was none.
func DeleteCredentials() error {
	p, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// New returns a client that acts as whoever is signed in.
func New() (*Client, error) {
	c, err := LoadCredentials()
	if err != nil {
		return nil, err
	}
	return &Client{Server: c.Server, Token: c.Token}, nil
}
