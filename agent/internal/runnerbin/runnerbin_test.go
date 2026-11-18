package runnerbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func tarball(t *testing.T, files map[string]string, link [2]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	if link[0] != "" {
		_ = tw.WriteHeader(&tar.Header{Name: link[0], Linkname: link[1], Typeflag: tar.TypeSymlink})
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func server(t *testing.T, archive []byte, sha string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/actions/runner/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.330.0",
				"body":     "<!-- BEGIN SHA osx-arm64 -->" + sha + "<!-- END SHA osx-arm64 -->",
				"assets": []map[string]string{{
					"name":                 "actions-runner-osx-arm64-2.330.0.tar.gz",
					"browser_download_url": srv.URL + "/dl",
				}},
			})
		case "/dl":
			_, _ = w.Write(archive)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateDownloadsVerifiesAndCaches(t *testing.T) {
	archive := tarball(t, map[string]string{"run.sh": "#!/bin/sh\n", "bin/Runner.Listener": "x"}, [2]string{})
	sum := sha256.Sum256(archive)
	srv := server(t, archive, hex.EncodeToString(sum[:]))
	m := New(t.TempDir(), "osx-arm64")
	m.APIURL = srv.URL
	dir, err := m.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "2.330.0" || m.Current() != dir {
		t.Fatalf("dir %s current %s", dir, m.Current())
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatal(err)
	}
	// A fresh manager finds the cached version through the symlink.
	m2 := New(m.Dir, "osx-arm64")
	if m2.Current() != dir {
		t.Fatalf("current after restart: %s", m2.Current())
	}
}

func TestUpdateRejectsBadChecksum(t *testing.T) {
	archive := tarball(t, map[string]string{"run.sh": "x"}, [2]string{})
	srv := server(t, archive, "0000000000000000000000000000000000000000000000000000000000000000")
	m := New(t.TempDir(), "osx-arm64")
	m.APIURL = srv.URL
	if _, err := m.Update(context.Background()); err == nil {
		t.Fatal("expected checksum error")
	}
	if m.Current() != "" {
		t.Fatal("bad download became current")
	}
}

func TestExtractRejectsEscapingSymlinks(t *testing.T) {
	archive := tarball(t, map[string]string{"run.sh": "x"}, [2]string{"evil", "../../etc/passwd"})
	sum := sha256.Sum256(archive)
	srv := server(t, archive, hex.EncodeToString(sum[:]))
	m := New(t.TempDir(), "osx-arm64")
	m.APIURL = srv.URL
	if _, err := m.Update(context.Background()); err == nil {
		t.Fatal("expected error for escaping symlink")
	}
}
