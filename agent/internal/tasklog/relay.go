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
//     (the one whose pin it was given), by Diffie-Hellman.
//   - The caller sends the task's secret sealed under a key made from that and a salt of its
//     choosing, bound to what it asks for: the task, where to start, and whether to follow.
//     The host opens it and checks it against the hash that came with the task, as the direct
//     listener does.
//   - The host answers with random bytes of its own. The output is sealed under a second key,
//     made from the agreed secret, the salt and those bytes, so every stream has a key of its
//     own. Were the key the same each time, a control plane that sent a caller's request
//     again, to start elsewhere, would be given other output sealed exactly as the first was,
//     which is enough to read both.
//   - The host sends the output in parts, and then how the task ended, each sealed under that
//     key. A stream that stops without its sealed ending was cut short.
//
// The control plane passes all of it on and holds neither the task's secret nor either
// private key, so it can read none of it, and cannot say how a task ended. As with the direct
// listener, that rests on the caller having the right pin for the host.

const (
	relayAuthInfo  = "mcc-output-relay-v2 auth"
	relayPartsInfo = "mcc-output-relay-v2 parts"
	// The host's random bytes for one stream.
	streamNonceSize = 32
)

// Directions, so that the two ends never seal under the same nonce.
const (
	fromCaller byte = 0
	fromHost   byte = 1
)

// Channel seals and opens what the two ends of a relayed stream send each other.
type Channel struct {
	aead cipher.AEAD
}

func newChannel(shared, salt []byte, info string) (*Channel, error) {
	key, err := hkdf.Key(sha256.New, shared, salt, info, 32)
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

// Summary is how a stream ended, as the host seals it: the task's exit code if it ran to one,
// or why it has none ("running" if the stream ended before the task did).
type Summary struct {
	ExitCode  *int   `json:"exit_code,omitempty"`
	Code      string `json:"code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SealEnd seals how a stream ended, after n parts that reached offset.
func (c *Channel) SealEnd(n uint64, offset int64, s Summary) string {
	plain, _ := json.Marshal(s)
	return c.seal(fromHost, n, endAbout(offset), plain)
}

func endAbout(offset int64) string { return "end\n" + strconv.FormatInt(offset, 10) }

// authAbout is what a caller's sealed secret is bound to: the request it was made for.
func authAbout(taskID string, offset int64, follow bool) string {
	return fmt.Sprintf("auth\n%s\n%d\n%t", taskID, offset, follow)
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

// HostChannel is the host's end: from its own key, what the caller sent to attach, and what
// the caller asked for. It returns the channel that seals this stream, the random bytes the
// caller needs to open it, and the task's secret the caller sealed.
func HostChannel(key *ecdsa.PrivateKey, clientKey, salt, auth, taskID string, offset int64, follow bool) (ch *Channel, streamNonce, secret string, err error) {
	priv, err := key.ECDH()
	if err != nil {
		return nil, "", "", err
	}
	rawKey, err := base64.StdEncoding.DecodeString(clientKey)
	if err != nil {
		return nil, "", "", err
	}
	pub, err := ecdh.P256().NewPublicKey(rawKey)
	if err != nil {
		return nil, "", "", err
	}
	rawSalt, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || len(rawSalt) < 16 {
		return nil, "", "", errors.New("the salt is too short")
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, "", "", err
	}
	authCh, err := newChannel(shared, rawSalt, relayAuthInfo)
	if err != nil {
		return nil, "", "", err
	}
	opened, err := authCh.open(fromCaller, 0, authAbout(taskID, offset, follow), auth)
	if err != nil {
		return nil, "", "", err
	}
	rawNonce := make([]byte, streamNonceSize)
	if _, err := rand.Read(rawNonce); err != nil {
		return nil, "", "", err
	}
	if ch, err = newChannel(shared, append(rawSalt, rawNonce...), relayPartsInfo); err != nil {
		return nil, "", "", err
	}
	return ch, base64.StdEncoding.EncodeToString(rawNonce), string(opened), nil
}

// callerEnd is the caller's end before the host has answered: enough to make the stream's
// channel once the host's random bytes arrive.
type callerEnd struct {
	shared, salt []byte
}

// channel is the channel of the stream the host began with these random bytes.
func (e *callerEnd) channel(streamNonce string) (*Channel, error) {
	raw, err := base64.StdEncoding.DecodeString(streamNonce)
	if err != nil || len(raw) != streamNonceSize {
		return nil, errors.New("the host did not begin the stream properly")
	}
	return newChannel(e.shared, append(append([]byte{}, e.salt...), raw...), relayPartsInfo)
}

// callerChannel is the caller's end: from the host's public key, which the caller has checked
// against its pin, and what it asks for. It returns what to send the host to attach.
func callerChannel(hostKey, secret, taskID string, offset int64, follow bool) (end *callerEnd, clientKey, salt, auth string, err error) {
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
	authCh, err := newChannel(shared, rawSalt, relayAuthInfo)
	if err != nil {
		return nil, "", "", "", err
	}
	return &callerEnd{shared: shared, salt: rawSalt}, base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()),
		base64.StdEncoding.EncodeToString(rawSalt), authCh.seal(fromCaller, 0, authAbout(taskID, offset, follow), []byte(secret)), nil
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
	caller, clientKey, salt, auth, err := callerChannel(task.Output.Key, secret, id, offset, follow)
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

	var ch *Channel
	var n uint64
	next := offset
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return end, fmt.Errorf("the stream broke after %d bytes: %w", end.Read, err)
		}
		var f struct {
			Nonce   string `json:"nonce"`
			Offset  int64  `json:"offset"`
			Data    string `json:"data"`
			End     bool   `json:"end"`
			Summary string `json:"summary"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return end, err
		}
		if f.Error != "" {
			return end, errors.New(f.Error)
		}
		if ch == nil {
			// The host's first word is the random bytes this stream's key is made with.
			if f.Nonce == "" || f.Data != "" || f.End {
				return end, errors.New("the host did not begin the stream properly; its agent may be older than this mcc")
			}
			if ch, err = caller.channel(f.Nonce); err != nil {
				return end, err
			}
			continue
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
			// How it ended is the host's to say, sealed after everything it sent: an ending that
			// does not open here was not the host's, or came before all the parts did.
			plain, err := ch.open(fromHost, n, endAbout(next), f.Summary)
			var sum Summary
			if err != nil || json.Unmarshal(plain, &sum) != nil {
				return end, fmt.Errorf("the stream was cut short after %d bytes: its ending was not sealed by the host", end.Read)
			}
			end.Truncated = sum.Truncated
			switch {
			case sum.Code == "running":
				end.Running = true
			case sum.Code != "":
				end.Code = sum.Code
			default:
				zero := 0
				end.ExitCode = &zero
				if sum.ExitCode != nil {
					end.ExitCode = sum.ExitCode
				}
			}
			return end, nil
		}
	}
}
