package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to the GitHub releases API. Repo is "owner/name"; Token
// authenticates private-repo release reads (ONEGW_GITHUB_TOKEN, then
// GITHUB_TOKEN).
type Client struct {
	Base  string // API base; default https://api.github.com
	Repo  string
	Token string
	HTTP  *http.Client
}

// Asset is one release artifact. APIURL is the asset API endpoint
// (/releases/assets/<id>): downloading through it with
// Accept: application/octet-stream works for public AND private repos,
// whereas browser_download_url ignores Bearer auth on private repos.
// APIURL redirects to a signed host for public assets; the client strips
// the Authorization header on the cross-host hop automatically.
type Asset struct {
	Name   string `json:"name"`
	APIURL string `json:"url"` // asset API endpoint; used for download
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // "sha256:...", when GitHub provides it
}

// Release is the newest published release of the repo.
type Release struct {
	Tag    string  `json:"tag_name"`
	Name   string  `json:"name,omitempty"`
	Notes  string  `json:"body,omitempty"`
	URL    string  `json:"html_url,omitempty"`
	Assets []Asset `json:"assets"`
}

// token returns the GitHub API token from the environment:
// ONEGW_GITHUB_TOKEN, then GITHUB_TOKEN.
func token() string {
	if t := os.Getenv("ONEGW_GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITHUB_TOKEN")
}

// resolveToken prefers an explicit Client.Token over the environment.
func resolveToken(c *Client) string {
	if c != nil && c.Token != "" {
		return c.Token
	}
	return token()
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	// ONEGW_RELEASE_API redirects ALL update traffic (checks, downloads,
	// admin-triggered applies) at a mirror — also the test hook for the
	// fake releases server.
	if b := os.Getenv("ONEGW_RELEASE_API"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.github.com"
}

// Latest fetches the newest published release. Any non-200 (including
// 404 for "no releases yet" or a private repo without a token) surfaces as
// an error carrying the status, so callers can log a precise reason.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.base(), c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "onegw/"+Version())
	if t := resolveToken(c); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: check %s: %w", c.Repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("update: check %s: HTTP %d %s", c.Repo, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update: decode release: %w", err)
	}
	if rel.Tag == "" {
		return nil, fmt.Errorf("update: release for %s has no tag", c.Repo)
	}
	return &rel, nil
}

// SelectAsset picks the release asset for the running platform; release
// CI publishes raw binaries named onegw-<goos>-<goarch>.
func (rel *Release) SelectAsset(goos, goarch string) (*Asset, error) {
	want := "onegw-" + goos + "-" + goarch
	for i := range rel.Assets {
		if rel.Assets[i].Name == want {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("update: release %s has no asset %s (assets: %s)",
		rel.Tag, want, assetNames(rel.Assets))
}

func assetNames(assets []Asset) string {
	names := make([]string, 0, len(assets))
	for _, a := range assets {
		names = append(names, a.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Download streams the asset into dst (a temp path on the target binary's
// filesystem) and verifies the sha256 digest GitHub publishes with the
// asset, when present. A mismatch removes the partial file and fails — a
// truncated download must never replace a serving binary.
func (a *Asset) Download(ctx context.Context, dst string) error {
	dl := a.APIURL
	if dl == "" {
		dl = a.URL // test fixtures / mirrors without the API endpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "onegw/"+Version())
	req.Header.Set("Accept", "application/octet-stream")
	if t := token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("update: download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download %s: HTTP %d", a.Name, resp.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("update: create %s: %w", tmp, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("update: download %s: %w", a.Name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if want := strings.TrimPrefix(a.Digest, "sha256:"); want != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			_ = os.Remove(tmp)
			return fmt.Errorf("update: %s checksum mismatch (got sha256:%s, want sha256:%s)", a.Name, got, want)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
