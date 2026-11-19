// Package api is the command line's client for the control plane's HTTP API, and where it
// keeps the token it was given by signing in.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultServer is the control plane the command line talks to when it is told no other.
const DefaultServer = "https://api.mycompute.cloud"

// Client calls the API as whoever its token is.
type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

// Error is an answer from the API that is not a success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the control plane answered %d", e.Status)
	}
	return e.Message
}

// IsStatus reports whether err is the API answering with that status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// Do sends in as JSON, if it is not nil, and decodes the answer into out, if it is not nil.
// It returns the answer's status.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Server, "/")+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reaching %s: %w", c.Server, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if res.StatusCode == http.StatusUnauthorized && e.Error == "unauthorized" {
			e.Error = "not signed in, or the sign-in has ended: run `mcc auth login`"
		}
		return res.StatusCode, &Error{Status: res.StatusCode, Message: e.Error}
	}
	if out != nil && len(raw) > 0 {
		if p, ok := out.(*json.RawMessage); ok {
			*p = raw
			return res.StatusCode, nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, fmt.Errorf("the control plane's answer could not be read: %w", err)
		}
	}
	return res.StatusCode, nil
}
