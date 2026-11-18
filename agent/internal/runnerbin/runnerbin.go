// Package runnerbin keeps a current GitHub Actions runner on the host. The agent shares it
// read-only into each VM, so a stale image never carries a runner version GitHub rejects.
package runnerbin

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Manager struct {
	Dir      string // cache directory; versions live in Dir/<version>
	Platform string // runner platform, for example osx-arm64
	APIURL   string // default https://api.github.com
	Client   *http.Client

	mu      sync.Mutex
	current string
}

func New(dir, platform string) *Manager {
	return &Manager{Dir: dir, Platform: platform, APIURL: "https://api.github.com", Client: http.DefaultClient}
}

// Current returns the directory of the newest runner ready on disk, or "" if none.
func (m *Manager) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != "" {
		return m.current
	}
	if target, err := os.Readlink(filepath.Join(m.Dir, "current")); err == nil {
		m.current = filepath.Join(m.Dir, target)
	}
	return m.current
}

type release struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Update downloads the latest runner release if it is not already cached, verifies its SHA-256
// against the release notes, and makes it current. Returns the runner directory.
func (m *Manager) Update(ctx context.Context) (string, error) {
	rel, err := m.latest(ctx)
	if err != nil {
		return m.Current(), err
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	dest := filepath.Join(m.Dir, version)
	if _, err := os.Stat(filepath.Join(dest, "run.sh")); err == nil {
		return dest, m.setCurrent(version)
	}
	name := fmt.Sprintf("actions-runner-%s-%s.tar.gz", m.Platform, version)
	var url string
	for _, a := range rel.Assets {
		if a.Name == name {
			url = a.URL
		}
	}
	if url == "" {
		return m.Current(), fmt.Errorf("release %s has no asset %s", rel.TagName, name)
	}
	want := expectedSHA(rel.Body, m.Platform)
	if want == "" {
		return m.Current(), fmt.Errorf("release %s does not state a SHA-256 for %s", rel.TagName, m.Platform)
	}
	if err := m.download(ctx, url, want, dest); err != nil {
		return m.Current(), err
	}
	return dest, m.setCurrent(version)
}

func (m *Manager) latest(ctx context.Context) (*release, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", m.APIURL+"/repos/actions/runner/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("fetching latest runner release: %s", res.Status)
	}
	var r release
	return &r, json.NewDecoder(res.Body).Decode(&r)
}

// Release notes carry lines like <!-- BEGIN SHA osx-arm64 -->abc...<!-- END SHA osx-arm64 -->.
func expectedSHA(body, platform string) string {
	re := regexp.MustCompile(`<!-- BEGIN SHA ` + regexp.QuoteMeta(platform) + ` -->([0-9a-f]{64})<!-- END SHA`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func (m *Manager) download(ctx context.Context, url, wantSHA, dest string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	res, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("downloading runner: %s", res.Status)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.Dir, "download-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), res.Body); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("runner checksum mismatch: got %s, want %s", got, wantSHA)
	}
	staging := dest + ".partial"
	_ = os.RemoveAll(staging)
	if err := extract(tmp.Name(), staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	return os.Rename(staging, dest)
}

func extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dest, hdr.Name)
		if !strings.HasPrefix(p, root) && p != filepath.Clean(dest) {
			return fmt.Errorf("archive entry escapes destination: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			target := filepath.Join(filepath.Dir(p), hdr.Linkname)
			if filepath.IsAbs(hdr.Linkname) || !strings.HasPrefix(target, root) {
				return fmt.Errorf("archive symlink escapes destination: %s", hdr.Name)
			}
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) setCurrent(version string) error {
	link := filepath.Join(m.Dir, "current")
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		return err
	}
	m.mu.Lock()
	m.current = filepath.Join(m.Dir, version)
	m.mu.Unlock()
	return nil
}
