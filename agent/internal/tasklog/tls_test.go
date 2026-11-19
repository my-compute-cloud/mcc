package tasklog

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// listener serves a store over TLS on a loopback port, and counts the requests that reach it.
func listener(t *testing.T, s *Store) (address, pin string, requests *atomic.Int32) {
	t.Helper()
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if err != nil {
		t.Fatal(err)
	}
	if pin, err = Pin(&key.PublicKey); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := Certificate(key, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	requests = &atomic.Int32{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inner := s.Handler(false)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		inner.ServeHTTP(w, r)
	})}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})) }()
	return ln.Addr().String(), pin, requests
}

func TestTheListenersKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.key")
	a, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := Pin(&a.PublicKey)
	pb, _ := Pin(&b.PublicKey)
	if pa != pb || len(pa) != 44 {
		t.Errorf("pins %q and %q", pa, pb)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if po, _ := Pin(&other.PublicKey); po == pa {
		t.Error("two hosts have the same key")
	}
}

func TestOutputIsReadFromAnotherMachineWithThePinAndTheSecret(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, _ := s.Create(Meta{ID: "t1", SecretHash: hashOf("s3cret")})
	_, _ = l.Write([]byte("built\n"))
	l.Close(intp(0), "")
	open, _ := s.Create(Meta{ID: "t2"})
	_, _ = open.Write([]byte("owner only\n"))
	open.Close(intp(0), "")

	address, pin, requests := listener(t, s)
	ctx := context.Background()
	var b bytes.Buffer
	end, err := ReadTLS(ctx, address, "sha256//"+pin, "t1", "s3cret", 0, true, &b)
	if err != nil || b.String() != "built\n" || end.ExitCode == nil || *end.ExitCode != 0 {
		t.Fatalf("with pin and secret: %q %+v %v", b.String(), end, err)
	}
	// The bare pin, without curl's prefix, is the same pin.
	b.Reset()
	if _, err := ReadTLS(ctx, address, pin, "t1", "s3cret", 3, false, &b); err != nil || b.String() != "lt\n" {
		t.Fatalf("from an offset: %q %v", b.String(), err)
	}

	for _, c := range []struct{ name, id, secret string }{
		{"a wrong secret", "t1", "guess"},
		{"a task whose caller chose no secret", "t2", "anything"},
		{"a task that never ran here", "t9", "s3cret"},
	} {
		b.Reset()
		if _, err := ReadTLS(ctx, address, pin, c.id, c.secret, 0, false, &b); err == nil || b.Len() != 0 {
			t.Errorf("%s: read %q, %v", c.name, b.String(), err)
		}
	}
	if _, err := ReadTLS(ctx, address, pin, "t1", "", 0, false, &b); err == nil {
		t.Error("a read with no secret was attempted")
	}
	if _, err := ReadTLS(ctx, address, "not a pin", "t1", "s3cret", 0, false, &b); err == nil {
		t.Error("a malformed pin was accepted")
	}

	// A server with another key gets no request, and so never sees the secret.
	impostor, _, seen := listener(t, s)
	before := requests.Load()
	b.Reset()
	_, err = ReadTLS(ctx, impostor, pin, "t1", "s3cret", 0, false, &b)
	if !errors.Is(err, ErrWrongKey) || b.Len() != 0 {
		t.Fatalf("a server with another key: %q, %v", b.String(), err)
	}
	if seen.Load() != 0 || requests.Load() != before {
		t.Error("a request was sent to a server that did not have the pinned key")
	}

	// An ordinary TLS client, which trusts authorities and not pins, does not get in either.
	if _, err := http.Get("https://" + address + "/v1/tasks/t1/output"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an unpinned client: %v", err)
	}
}

// The listener as the agent runs it, read by curl: the client the docs tell callers to use.
func TestTheListenerServesCurlWithAPinnedKey(t *testing.T) {
	s := New(t.TempDir(), Limits{})
	l, _ := s.Create(Meta{ID: "t1", SecretHash: hashOf("s3cret")})
	_, _ = l.Write([]byte("built\n"))
	l.Close(intp(4), "")

	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := Pin(&key.PublicKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	cert, _ := Certificate(key, address)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.ServeTLS(ctx, ln, cert) }()

	var b bytes.Buffer
	end, err := ReadTLS(ctx, address, pin, "t1", "s3cret", 0, true, &b)
	if err != nil || b.String() != "built\n" || end.ExitCode == nil || *end.ExitCode != 4 {
		t.Fatalf("ReadTLS: %q %+v %v", b.String(), end, err)
	}

	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	run := func(pin string) (string, error) {
		out, err := exec.Command(curl, "-sS", "--fail", "-k", "--noproxy", "*", "--max-time", "10",
			"--pinnedpubkey", "sha256//"+pin, "-H", "Authorization: Bearer s3cret",
			"https://"+address+"/v1/tasks/t1/output").CombinedOutput()
		return string(out), err
	}
	if out, err := run(pin); err != nil || out != "built\n" {
		t.Errorf("curl with the pin: %q, %v", out, err)
	}
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	wrong, _ := Pin(&other.PublicKey)
	if out, err := run(wrong); err == nil {
		t.Errorf("curl with another key's pin read %q", out)
	}
}
