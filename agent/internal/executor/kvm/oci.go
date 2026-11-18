package kvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// An image is an OCI artifact: one manifest whose config says what the disk needs, and whose
// layers, in order, are the pieces of one qcow2 disk. A tag may also name an index with one
// such manifest per architecture, so the same reference serves x86_64 and arm64 hosts.
const (
	mediaConfig = "application/vnd.mcc.vm.config.v1+json"
	mediaDisk   = "application/vnd.mcc.vm.disk.v1.qcow2"

	mediaManifest       = "application/vnd.oci.image.manifest.v1+json"
	mediaIndex          = "application/vnd.oci.image.index.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTartConfig     = "application/vnd.cirruslabs.tart.config.v1"
)

// ImageConfig is what an image says about itself: who jobs run as, which is whom the host logs
// in as and whose files a writable share's are.
type ImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	User         string `json:"user"`
	UID          int    `json:"uid"`
	GID          int    `json:"gid"`
}

var userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func (c ImageConfig) validate(arch string) error {
	if c.OS != "" && c.OS != "linux" {
		return fmt.Errorf("the image is for %s guests, not linux", c.OS)
	}
	if c.Architecture != arch {
		return fmt.Errorf("the image is for %s, and this host is %s", c.Architecture, arch)
	}
	if !userName.MatchString(c.User) || c.UID <= 0 || c.GID <= 0 {
		return errors.New("the image's config does not name the job's user, uid and gid")
	}
	return nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
}

type reference struct {
	host, repo, tag, digest string
}

var (
	refPart   = regexp.MustCompile(`^[a-z0-9]+(?:[._-]+[a-z0-9]+)*$`)
	refTag    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	refDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refHost   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?$`)
)

// parseReference reads "host/path[:tag][@sha256:...]". There is no default registry: a source
// always says where it lives.
func parseReference(s string) (reference, error) {
	var r reference
	bad := fmt.Errorf("%q is not an image reference like ghcr.io/owner/name:tag", s)
	if at := strings.Index(s, "@"); at >= 0 {
		r.digest, s = s[at+1:], s[:at]
		if !refDigest.MatchString(r.digest) {
			return r, bad
		}
	}
	slash := strings.Index(s, "/")
	if slash <= 0 {
		return r, bad
	}
	r.host, s = s[:slash], s[slash+1:]
	if colon := strings.LastIndex(s, ":"); colon >= 0 {
		r.tag, s = s[colon+1:], s[:colon]
		if !refTag.MatchString(r.tag) {
			return r, bad
		}
	}
	r.repo = s
	if !refHost.MatchString(r.host) || r.repo == "" {
		return r, bad
	}
	for _, part := range strings.Split(r.repo, "/") {
		if !refPart.MatchString(part) {
			return r, bad
		}
	}
	if r.tag == "" && r.digest == "" {
		r.tag = "latest"
	}
	return r, nil
}

// base is the registry's URL. A registry on this machine is spoken to without TLS, as
// container tools do, so that an owner can keep images in a local registry.
func (r reference) base() string {
	host := r.host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host == "localhost" || host == "127.0.0.1" {
		return "http://" + r.host
	}
	return "https://" + r.host
}

func (r reference) version() string {
	if r.digest != "" {
		return r.digest
	}
	return r.tag
}

// registry fetches from one repository, with the anonymous token that public images need.
type registry struct {
	client *http.Client
	ref    reference
	token  string
}

func (g *registry) get(ctx context.Context, path string, accept ...string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.ref.base()+"/v2/"+g.ref.repo+path, nil)
		if err != nil {
			return nil, err
		}
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
		res, err := g.client.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := res.Header.Get("WWW-Authenticate")
			res.Body.Close()
			if err := g.authorize(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("%s%s: %s", g.ref.host+"/"+g.ref.repo, path, res.Status)
		}
		return res, nil
	}
}

var challengeParam = regexp.MustCompile(`(realm|service|scope)="([^"]*)"`)

// authorize asks the registry's token service for an anonymous pull token.
func (g *registry) authorize(ctx context.Context, challenge string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return fmt.Errorf("%s wants a login, and only images anyone can pull are supported", g.ref.host)
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" && !strings.HasPrefix(g.ref.base(), "http://") {
		return fmt.Errorf("%s names no usable token service", g.ref.host)
	}
	q := realm.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + g.ref.repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	res, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/%s cannot be pulled without a login: %s", g.ref.host, g.ref.repo, res.Status)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return err
	}
	if g.token = out.Token; g.token == "" {
		g.token = out.AccessToken
	}
	if g.token == "" {
		return fmt.Errorf("%s gave no pull token", g.ref.host)
	}
	return nil
}

// small reads a manifest or config, which are a few kilobytes, and returns it with its digest.
func (g *registry) small(ctx context.Context, path, wantDigest string, accept ...string) ([]byte, string, error) {
	res, err := g.get(ctx, path, accept...)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if wantDigest != "" && digest != wantDigest {
		return nil, "", fmt.Errorf("%s%s is not what its digest says: got %s", g.ref.repo, path, digest)
	}
	return b, digest, nil
}

// resolved is an image as the registry has it now.
type resolved struct {
	// Digest of what the reference names, an index or a manifest: it changes when the tag moves.
	Digest string
	Config ImageConfig
	Layers []descriptor
}

// resolve finds this host's manifest for a reference and reads its config, without fetching
// the disk.
func (g *registry) resolve(ctx context.Context, arch string) (resolved, error) {
	var out resolved
	accept := []string{mediaIndex, mediaManifest, mediaDockerIndex, mediaDockerManifest}
	body, digest, err := g.small(ctx, "/manifests/"+g.ref.version(), g.ref.digest, accept...)
	if err != nil {
		return out, err
	}
	out.Digest = digest
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return out, fmt.Errorf("reading the image's manifest: %w", err)
	}
	if len(m.Manifests) > 0 {
		var pick *descriptor
		for i, d := range m.Manifests {
			if d.Platform != nil && d.Platform.Architecture == arch && (d.Platform.OS == "linux" || d.Platform.OS == "") {
				pick = &m.Manifests[i]
				break
			}
		}
		if pick == nil || !refDigest.MatchString(pick.Digest) {
			return out, fmt.Errorf("the image has no disk for %s hosts", arch)
		}
		body, _, err = g.small(ctx, "/manifests/"+pick.Digest, pick.Digest, mediaManifest, mediaDockerManifest)
		if err != nil {
			return out, err
		}
		m = manifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return out, fmt.Errorf("reading the image's manifest: %w", err)
		}
	}
	switch {
	case m.Config.MediaType == mediaTartConfig:
		return out, errors.New("this is an image for Tart, which only Mac hosts run; a Linux host needs the image's KVM disk")
	case m.Config.MediaType != mediaConfig || !refDigest.MatchString(m.Config.Digest):
		return out, errors.New("this is not a virtual machine image for Linux hosts")
	}
	body, _, err = g.small(ctx, "/blobs/"+m.Config.Digest, m.Config.Digest)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out.Config); err != nil {
		return out, fmt.Errorf("reading the image's config: %w", err)
	}
	if err := out.Config.validate(arch); err != nil {
		return out, err
	}
	for _, l := range m.Layers {
		if l.MediaType != mediaDisk || !refDigest.MatchString(l.Digest) || l.Size <= 0 {
			return out, errors.New("the image has a layer that is not a piece of a qcow2 disk")
		}
		out.Layers = append(out.Layers, l)
	}
	if len(out.Layers) == 0 {
		return out, errors.New("the image has no disk")
	}
	return out, nil
}

// download writes the image's disk to path: its layers one after another, each checked against
// its digest as it arrives.
func (g *registry) download(ctx context.Context, layers []descriptor, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range layers {
		res, err := g.get(ctx, "/blobs/"+l.Digest)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, l.Size+1))
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("downloading the image: %w", err)
		}
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); n != l.Size || got != l.Digest {
			return fmt.Errorf("a piece of the image is not what its digest says: got %s (%d bytes), want %s (%d bytes)", got, n, l.Digest, l.Size)
		}
	}
	return f.Sync()
}

// localImage is a source that names a directory on this host instead of a registry: what the
// image build leaves, a disk.qcow2 beside its config.json. Its "digest" changes when either
// file does.
func localImage(dir string) (resolved, error) {
	var out resolved
	b, err := os.ReadFile(dir + "/config.json")
	if err != nil {
		return out, err
	}
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&out.Config); err != nil {
		return out, fmt.Errorf("%s/config.json: %w", dir, err)
	}
	st, err := os.Stat(dir + "/disk.qcow2")
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(fmt.Appendf(b, "\n%d %d", st.Size(), st.ModTime().UnixNano()))
	out.Digest = "local:" + hex.EncodeToString(sum[:])
	return out, nil
}
