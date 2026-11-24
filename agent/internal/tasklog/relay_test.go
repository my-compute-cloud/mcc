package tasklog

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestTheTwoEndsOfARelayedStreamAgreeAndNoOneElseDoes(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _ := PublicKey(key)
	pin, _ := Pin(&key.PublicKey)
	if !keyMatchesPin(hostKey, pin) || !keyMatchesPin(hostKey, "sha256//"+pin) {
		t.Fatal("a key does not match its own pin")
	}

	end, clientKey, salt, auth, err := callerChannel(hostKey, "s3cret", "t1", 100, true)
	if err != nil {
		t.Fatal(err)
	}
	host, streamNonce, secret, err := HostChannel(key, clientKey, salt, auth, "t1", 100, true)
	if err != nil || secret != "s3cret" {
		t.Fatalf("the host read %q, %v", secret, err)
	}
	caller, err := end.channel(streamNonce)
	if err != nil {
		t.Fatal(err)
	}
	part := host.SealPart(0, 100, []byte("built\n"))
	if got, err := caller.open(fromHost, 0, "100", part); err != nil || string(got) != "built\n" {
		t.Fatalf("the caller read %q, %v", got, err)
	}
	// A part does not open as another part, at another place, or as something the caller said.
	if _, err := caller.open(fromHost, 1, "100", part); err == nil {
		t.Error("a part opened out of order")
	}
	if _, err := caller.open(fromHost, 0, "0", part); err == nil {
		t.Error("a part opened at another offset")
	}
	if _, err := caller.open(fromCaller, 0, "100", part); err == nil {
		t.Error("a part from the host opened as one from the caller")
	}
	// How it ended opens only after all the parts, where they stopped.
	zero := 0
	ending := host.SealEnd(1, 106, Summary{ExitCode: &zero})
	if _, err := caller.open(fromHost, 1, endAbout(106), ending); err != nil {
		t.Errorf("the ending did not open: %v", err)
	}
	if _, err := caller.open(fromHost, 0, endAbout(100), ending); err == nil {
		t.Error("the ending opened before the part it follows")
	}

	// Another host, which is what a control plane that swapped the key would be, reads nothing.
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if _, _, _, err := HostChannel(other, clientKey, salt, auth, "t1", 100, true); err == nil {
		t.Error("a host with another key opened the caller's secret")
	}
	otherKey, _ := PublicKey(other)
	if keyMatchesPin(otherKey, pin) {
		t.Error("another key matches the pin")
	}
	// What the caller sent cannot be used again with a salt of someone else's choosing, nor to
	// ask for something else: another task, another place to start, or not to follow.
	_, _, salt2, _, _ := callerChannel(hostKey, "x", "t1", 100, true)
	if _, _, _, err := HostChannel(key, clientKey, salt2, auth, "t1", 100, true); err == nil {
		t.Error("the sealed secret opened under another salt")
	}
	for _, asks := range []struct {
		task   string
		offset int64
		follow bool
	}{{"t2", 100, true}, {"t1", 101, true}, {"t1", 100, false}} {
		if _, _, _, err := HostChannel(key, clientKey, salt, auth, asks.task, asks.offset, asks.follow); err == nil {
			t.Errorf("the sealed secret opened for another request: %+v", asks)
		}
	}
	for _, bad := range []struct{ clientKey, salt, auth string }{
		{"not base64!", salt, auth},
		{clientKey, "c2hvcnQ=", auth},
		{clientKey, salt, "AAAA"},
	} {
		if _, _, _, err := HostChannel(key, bad.clientKey, bad.salt, bad.auth, "t1", 100, true); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, bad := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := end.channel(bad); err == nil {
			t.Errorf("made a channel from the host's bytes %q", bad)
		}
	}
}

// The control plane carries a caller's request to attach, and can send it to the host again.
// Each time the host must seal under a key of that stream's own: the same key twice, over
// different output, would give the output away.
func TestARequestSentAgainIsAnsweredUnderAnotherKey(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _ := PublicKey(key)
	end, clientKey, salt, auth, err := callerChannel(hostKey, "s3cret", "t1", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	first, nonce1, _, err := HostChannel(key, clientKey, salt, auth, "t1", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	again, nonce2, _, err := HostChannel(key, clientKey, salt, auth, "t1", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if nonce1 == nonce2 {
		t.Fatal("the host began two streams with the same random bytes")
	}
	a, b := first.SealPart(0, 0, []byte("the same words")), again.SealPart(0, 0, []byte("the same words"))
	if a == b {
		t.Fatal("two streams sealed the same part the same way: they share a key")
	}
	// The caller, who was given the first stream's bytes, cannot open the second's parts.
	caller, _ := end.channel(nonce1)
	if _, err := caller.open(fromHost, 0, "0", a); err != nil {
		t.Fatalf("the caller could not open its own stream: %v", err)
	}
	if _, err := caller.open(fromHost, 0, "0", b); err == nil {
		t.Error("a part of another stream opened under this stream's key")
	}
}

// A control plane between a caller and a host, which carries the stream as the real one does
// and may meddle with it on the way: meddle is given each frame from the host and returns
// what to pass on.
func relayThrough(t *testing.T, key *ecdsa.PrivateKey, output string, meddle func(frame map[string]any) []map[string]any) *httptest.Server {
	t.Helper()
	hostKey, _ := PublicKey(key)
	pin, _ := Pin(&key.PublicKey)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"key": hostKey, "pin": "sha256//" + pin}})
	})
	mux.HandleFunc("GET /api/v1/tasks/t1/output", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var attach struct {
			Offset    int64  `json:"offset"`
			Follow    bool   `json:"follow"`
			ClientKey string `json:"client_key"`
			Salt      string `json:"salt"`
			Auth      string `json:"auth"`
		}
		write := func(v any) error {
			b, _ := json.Marshal(v)
			return conn.Write(r.Context(), websocket.MessageText, b)
		}
		if _, raw, err := conn.Read(r.Context()); err != nil || json.Unmarshal(raw, &attach) != nil {
			return
		}
		// The host's part, as agent/relay.go plays it.
		ch, nonce, _, err := HostChannel(key, attach.ClientKey, attach.Salt, attach.Auth, "t1", attach.Offset, attach.Follow)
		if err != nil {
			_ = write(map[string]any{"error": "refused"})
			return
		}
		zero := 0
		body := []byte(output)[attach.Offset:]
		frames := []map[string]any{
			{"nonce": nonce},
			{"offset": attach.Offset, "data": ch.SealPart(0, attach.Offset, body)},
			{"end": true, "summary": ch.SealEnd(1, attach.Offset+int64(len(body)), Summary{ExitCode: &zero})},
		}
		for _, f := range frames {
			for _, out := range meddle(f) {
				if err := write(out); err != nil {
					return
				}
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOutputReadThroughTheControlPlaneIsTheHostsOrAnError(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if err != nil {
		t.Fatal(err)
	}
	pass := func(f map[string]any) []map[string]any { return []map[string]any{f} }
	read := func(meddle func(map[string]any) []map[string]any) (string, End, error) {
		srv := relayThrough(t, key, "hello\nworld\n", meddle)
		var out bytes.Buffer
		end, err := ReadRelay(context.Background(), nil, srv.URL, "token", "t1", "s3cret", "", 0, false, &out)
		return out.String(), end, err
	}

	out, end, err := read(pass)
	if err != nil || out != "hello\nworld\n" || end.ExitCode == nil || *end.ExitCode != 0 {
		t.Fatalf("an honest relay: %q, %+v, %v", out, end, err)
	}

	cases := []struct {
		name   string
		meddle func(map[string]any) []map[string]any
		want   string
	}{
		{"ends the stream itself, before the host did", func(f map[string]any) []map[string]any {
			if f["data"] != nil {
				return []map[string]any{{"end": true}}
			}
			return pass(f)
		}, "cut short"},
		{"gives the task another exit code", func(f map[string]any) []map[string]any {
			if f["end"] == true {
				return []map[string]any{{"end": true, "exit_code": 0, "code": "", "summary": ""}}
			}
			return pass(f)
		}, "cut short"},
		{"drops a part and passes on the ending", func(f map[string]any) []map[string]any {
			if f["data"] != nil {
				return nil
			}
			return pass(f)
		}, "cut short"},
		{"keeps the host's random bytes to itself", func(f map[string]any) []map[string]any {
			if f["nonce"] != nil {
				return nil
			}
			return pass(f)
		}, "did not begin the stream properly"},
		{"swaps the host's random bytes for its own", func(f map[string]any) []map[string]any {
			if f["nonce"] != nil {
				return []map[string]any{{"nonce": base64.StdEncoding.EncodeToString(make([]byte, streamNonceSize))}}
			}
			return pass(f)
		}, "not sealed by the host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := read(c.meddle)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("a control plane that %s was believed: %v", c.name, err)
			}
		})
	}
}
