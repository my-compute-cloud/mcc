package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Login is a sign-in that has been started and waits for its person's answer in a browser.
type Login struct {
	DeviceCode  string `json:"device_code"`
	UserCode    string `json:"user_code"`
	URL         string `json:"verification_url"`
	URLWithCode string `json:"verification_url_complete"`
	ExpiresIn   int    `json:"expires_in"`
	Interval    int    `json:"interval"`
}

// StartLogin asks the control plane at server for a sign-in. hostName is shown to the person
// who approves it; readOnly asks for a token that can change nothing.
func StartLogin(ctx context.Context, server, hostName string, readOnly bool) (Login, error) {
	var l Login
	c := &Client{Server: server}
	_, err := c.Do(ctx, http.MethodPost, "/api/v1/cli/logins", map[string]any{"host_name": hostName, "read_only": readOnly}, &l)
	return l, err
}

// ErrLoginRefused is returned when the person said no.
var ErrLoginRefused = errors.New("the sign-in was refused in the browser")

// ErrLoginExpired is returned when nobody answered in time.
var ErrLoginExpired = errors.New("the sign-in was not approved in time; run `mcc auth login` again")

// AwaitLogin asks until the sign-in is answered, and returns the credentials it was given.
func AwaitLogin(ctx context.Context, server string, l Login) (Credentials, error) {
	c := &Client{Server: server}
	interval := time.Duration(l.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(time.Duration(l.ExpiresIn) * time.Second)
	for {
		var out struct {
			Token     string    `json:"api_token"`
			User      string    `json:"user"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		status, err := c.Do(ctx, http.MethodPost, "/api/v1/cli/logins/token", map[string]string{"device_code": l.DeviceCode}, &out)
		switch {
		case err == nil && status == http.StatusOK && out.Token != "":
			return Credentials{Server: server, Token: out.Token, User: out.User, ExpiresAt: out.ExpiresAt}, nil
		case IsStatus(err, http.StatusForbidden):
			return Credentials{}, ErrLoginRefused
		case IsStatus(err, http.StatusGone):
			return Credentials{}, ErrLoginExpired
		case err != nil && ctx.Err() != nil:
			return Credentials{}, ctx.Err()
		}
		// Still waiting, or a passing failure to reach the control plane: ask again.
		if time.Now().After(deadline) {
			return Credentials{}, ErrLoginExpired
		}
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}
