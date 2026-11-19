package tasklog

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

// The relay carries a task's output from its host to a caller through the control plane, for
// hosts no caller can reach. What passes through is sealed between the two ends:
//
//   - The caller makes a one-time P-256 key and agrees a secret with the host's output key
//     (the one whose pin it was given), by Diffie-Hellman. From that and a salt of the
//     caller's choosing both ends derive one AES-256-GCM key.
//   - The caller sends the task's secret sealed under that key. The host opens it and checks
//     it against the hash that came with the task, as the direct listener does.
//   - The host sends the output in parts, each sealed under the same key.
//
// The control plane passes all of it on and holds neither the task's secret nor either
// private key, so it can read none of it. As with the direct listener, that rests on the
// caller having the right pin for the host.

const relayInfo = "mcc-output-relay-v1"

// Directions, so that the two ends never seal under the same nonce.
const (
	fromCaller byte = 0
	fromHost   byte = 1
)

// Channel seals and opens what the two ends of a relayed stream send each other.
type Channel struct {
	aead cipher.AEAD
}

func newChannel(shared, salt []byte) (*Channel, error) {
	key, err := hkdf.Key(sha256.New, shared, salt, relayInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Channel{aead: aead}, nil
}

func nonce(direction byte, n uint64) []byte {
	b := make([]byte, 12)
	b[0] = direction
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// seal seals the n-th message in a direction. about is bound to it without being hidden: a
// part's offset, so that a part cannot be passed off as belonging somewhere else.
func (c *Channel) seal(direction byte, n uint64, about string, plain []byte) string {
	return base64.StdEncoding.EncodeToString(c.aead.Seal(nil, nonce(direction, n), plain, []byte(about)))
}

func (c *Channel) open(direction byte, n uint64, about, sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	return c.aead.Open(nil, nonce(direction, n), raw, []byte(about))
}

// SealPart seals the n-th part of a stream's output, which starts at offset.
func (c *Channel) SealPart(n uint64, offset int64, part []byte) string {
	return c.seal(fromHost, n, strconv.FormatInt(offset, 10), part)
}

// PublicKey is a host's output key as callers are given it: base64 of its
// SubjectPublicKeyInfo. Its pin is the SHA-256 of the same bytes.
func PublicKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// HostChannel is the host's end: from its own key, and what the caller sent to attach. It
// returns the channel and the task's secret the caller sealed.
func HostChannel(key *ecdsa.PrivateKey, clientKey, salt, auth string) (*Channel, string, error) {
	priv, err := key.ECDH()
	if err != nil {
		return nil, "", err
	}
	rawKey, err := base64.StdEncoding.DecodeString(clientKey)
	if err != nil {
		return nil, "", err
	}
	pub, err := ecdh.P256().NewPublicKey(rawKey)
	if err != nil {
		return nil, "", err
	}
	rawSalt, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || len(rawSalt) < 16 {
		return nil, "", errors.New("the salt is too short")
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, "", err
	}
	ch, err := newChannel(shared, rawSalt)
	if err != nil {
		return nil, "", err
	}
	secret, err := ch.open(fromCaller, 0, "auth", auth)
	if err != nil {
		return nil, "", err
	}
	return ch, string(secret), nil
}

// callerChannel is the caller's end: from the host's public key, which the caller has checked
// against its pin. It returns the channel and what to send the host to attach.
func callerChannel(hostKey, secret string) (ch *Channel, clientKey, salt, auth string, err error) {
	der, err := base64.StdEncoding.DecodeString(hostKey)
	if err != nil {
		return nil, "", "", "", err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, "", "", "", err
	}
	ec, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", "", "", errors.New("the host's key is not a P-256 key")
	}
	hostPub, err := ec.ECDH()
	if err != nil {
		return nil, "", "", "", err
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", "", "", err
	}
	rawSalt := make([]byte, 32)
	if _, err := rand.Read(rawSalt); err != nil {
		return nil, "", "", "", err
	}
	shared, err := priv.ECDH(hostPub)
	if err != nil {
		return nil, "", "", "", err
	}
	if ch, err = newChannel(shared, rawSalt); err != nil {
		return nil, "", "", "", err
	}
	return ch, base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), base64.StdEncoding.EncodeToString(rawSalt),
		ch.seal(fromCaller, 0, "auth", []byte(secret)), nil
}

// keyMatchesPin reports whether a public key, as callers are given it, is the one a pin names.
func keyMatchesPin(hostKey, pin string) bool {
	der, err := base64.StdEncoding.DecodeString(hostKey)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pin, "sha256//"))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256(der)
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// ErrNoRelay is returned when a task's output cannot be read through the control plane.
var ErrNoRelay = errors.New("that task's output cannot be read through the control plane: it has no output secret, or its host keeps no output")

// ReadRelay copies a task's output to w through the control plane at server. token is an API
// token that may read the task; secret is the task's own. pin, if not empty, is the host's
// pin as the caller already has it, from the host's owner; otherwise the control plane's word
// for it is taken.
func ReadRelay(ctx context.Context, client *http.Client, server, token, id, secret, pin string, offset int64, follow bool, w io.Writer) (End, error) {
	var end End
	if !idPattern.MatchString(id) {
		return end, ErrNotFound
	}
	if secret == "" {
		return end, errors.New("reading through the control plane needs the task's secret")
	}
	if client == nil {
		client = http.DefaultClient
	}
	server = strings.TrimSuffix(server, "/")

	// Which host has the output, and its key.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/v1/tasks/"+id, nil)
	if err != nil {
		return end, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return end, err
	}
	var task struct {
		Error  string `json:"error"`
		Output struct {
			Key string `json:"key"`
			Pin string `json:"pin"`
		} `json:"output"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&task)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return end, fmt.Errorf("asking the control plane about the task: %s %s", res.Status, task.Error)
	}
	if err != nil {
		return end, err
	}
	if task.Output.Key == "" {
		return end, ErrNoRelay
	}
	if pin == "" {
		pin = task.Output.Pin
	}
	// The key the output will be sealed to must be the one the pin names. With the caller's own
	// pin, this is what keeps a control plane that lied about the key from reading anything.
	if !keyMatchesPin(task.Output.Key, pin) {
		return end, ErrWrongKey
	}
	ch, clientKey, salt, auth, err := callerChannel(task.Output.Key, secret)
	if err != nil {
		return end, err
	}

	ws := "ws" + strings.TrimPrefix(server, "http") + "/api/v1/tasks/" + id + "/output"
	conn, _, err := websocket.Dial(ctx, ws, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		return end, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	attach, _ := json.Marshal(map[string]any{
		"type": "attach", "offset": offset, "follow": follow, "client_key": clientKey, "salt": salt, "auth": auth,
	})
	if err := conn.Write(ctx, websocket.MessageText, attach); err != nil {
		return end, err
	}

	var n uint64
	next := offset
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return end, fmt.Errorf("the stream broke after %d bytes: %w", end.Read, err)
		}
		var f struct {
			Offset    int64  `json:"offset"`
			Data      string `json:"data"`
			End       bool   `json:"end"`
			ExitCode  *int   `json:"exit_code"`
			Code      string `json:"code"`
			Truncated bool   `json:"truncated"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return end, err
		}
		if f.Error != "" {
			return end, errors.New(f.Error)
		}
		if f.Data != "" {
			// A part is accepted only where the last one ended, and only if it opens under the key:
			// the control plane can drop the stream, but cannot reorder, repeat or alter it unseen.
			if f.Offset != next {
				return end, errors.New("the stream arrived out of order")
			}
			part, err := ch.open(fromHost, n, strconv.FormatInt(f.Offset, 10), f.Data)
			if err != nil {
				return end, errors.New("part of the stream was not sealed by the host")
			}
			n++
			next += int64(len(part))
			if _, err := w.Write(part); err != nil {
				return end, err
			}
			end.Read += int64(len(part))
		}
		if f.End {
			end.Truncated = f.Truncated
			switch {
			case f.Code == "running":
				end.Running = true
			case f.Code != "":
				end.Code = f.Code
			default:
				zero := 0
				end.ExitCode = &zero
				if f.ExitCode != nil {
					end.ExitCode = f.ExitCode
				}
			}
			return end, nil
		}
	}
}
