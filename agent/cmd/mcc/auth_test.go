package main

import "testing"

func TestOnlyAWebPageIsOpenedInTheBrowser(t *testing.T) {
	for _, c := range []struct {
		page, server string
		want         bool
	}{
		{"https://mycompute.cloud/device?code=ABCD-EFGH", "https://api.mycompute.cloud", true},
		{"https://elsewhere.example/device", "https://api.mycompute.cloud", true},
		{"HTTPS://mycompute.cloud/device", "https://api.mycompute.cloud", true},
		{"http://mycompute.cloud/device", "https://api.mycompute.cloud", false},
		{"http://localhost:8787/device", "http://localhost:8787", true},
		{"http://127.0.0.1:8787/device", "http://localhost:8787", true},
		{"http://[::1]:8787/device", "http://127.0.0.1:8787", true},
		{"http://site.localhost:5173/device", "http://localhost:8787", true},
		{"http://localhost:8787/device", "https://api.mycompute.cloud", false},
		{"http://mycompute.cloud/device", "http://localhost:8787", false},
		{"http://localhost.example/device", "http://localhost:8787", false},
		{"file:///Applications/Calculator.app", "https://api.mycompute.cloud", false},
		{"/Applications/Calculator.app", "https://api.mycompute.cloud", false},
		{"ssh://host/", "https://api.mycompute.cloud", false},
		{"javascript:alert(1)", "https://api.mycompute.cloud", false},
		{"https:///device", "https://api.mycompute.cloud", false},
		{"https://mycompute.cloud@evil.example/device", "https://api.mycompute.cloud", false},
		{"-a Calculator", "https://api.mycompute.cloud", false},
		{"", "https://api.mycompute.cloud", false},
	} {
		if got := mayOpen(c.page, c.server); got != c.want {
			t.Errorf("%q with server %q: got %v, want %v", c.page, c.server, got, c.want)
		}
	}
}
