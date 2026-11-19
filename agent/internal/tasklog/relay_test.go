package tasklog

import (
	"path/filepath"
	"testing"
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

	caller, clientKey, salt, auth, err := callerChannel(hostKey, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	host, secret, err := HostChannel(key, clientKey, salt, auth)
	if err != nil || secret != "s3cret" {
		t.Fatalf("the host read %q, %v", secret, err)
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

	// Another host, which is what a control plane that swapped the key would be, reads nothing.
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "output.key"))
	if _, _, err := HostChannel(other, clientKey, salt, auth); err == nil {
		t.Error("a host with another key opened the caller's secret")
	}
	otherKey, _ := PublicKey(other)
	if keyMatchesPin(otherKey, pin) {
		t.Error("another key matches the pin")
	}
	// What the caller sent cannot be used again with a salt of someone else's choosing.
	_, _, salt2, _, _ := callerChannel(hostKey, "x")
	if _, _, err := HostChannel(key, clientKey, salt2, auth); err == nil {
		t.Error("the sealed secret opened under another salt")
	}
	for _, bad := range []struct{ clientKey, salt, auth string }{
		{"not base64!", salt, auth},
		{clientKey, "c2hvcnQ=", auth},
		{clientKey, salt, "AAAA"},
	} {
		if _, _, err := HostChannel(key, bad.clientKey, bad.salt, bad.auth); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
