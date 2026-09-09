package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to the GitHub releases API. Repo is "owner/name"; Token
// authenticates private-repo release reads (ONEGW_GITHUB_TOKEN, then
// GITHUB_TOKEN). A token rejected with 401 is retried anonymously, so
// public repos never depend on the environment's token being valid.
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
// 404 for "no releases yet" or a private repo without a valid token)
// surfaces as an error carrying the status, so callers can log a precise
// reason. A rejected token (401) is first retried anonymously — see
// issueGET — so a public repo's check never depends on the environment's
// token being valid.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.base(), c.Repo)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	tok := resolveToken(c)
	resp, tokenRejected, err := issueGET(ctx, hc, url, "application/vnd.github+json", tok)
	if err != nil {
		return nil, fmt.Errorf("update: check %s: %w", c.Repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := fmt.Sprintf("update: check %s: HTTP %d %s", c.Repo, resp.StatusCode, strings.TrimSpace(string(body)))
		// 401/403/404 on the releases endpoint is almost always a token
		// problem, not a missing release: private repos 404 WITHOUT a
		// token and 401 WITH a rejected one. Say which fix applies.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			switch {
			case tokenRejected:
				// The token was rejected AND the anonymous fallback also
				// failed: a private repo behind an invalid credential.
				msg += " (credentials rejected AND anonymous access failed: refresh ONEGW_GITHUB_TOKEN/GITHUB_TOKEN — private repos need a valid token)"
			case tok != "":
				msg += " (credentials rejected? refresh the token, or unset ONEGW_GITHUB_TOKEN/GITHUB_TOKEN to fall back to the public API)"
			default:
				msg += " (private repo? set ONEGW_GITHUB_TOKEN or GITHUB_TOKEN to read its releases)"
			}
		}
		return nil, errors.New(msg)
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

// issueGET performs a GET against url with onegw's GitHub headers,
// optionally Bearer-authenticated with tok. When the authenticated
// request is rejected with 401 ("Bad credentials" — a stale, expired, or
// revoked token inherited from the environment), the SAME request is
// retried once WITHOUT credentials: GitHub serves public repositories
// like FreePeak/onegw to anonymous callers, so a broken token must never
// block a release read the public API would answer. tokenRejected
// reports whether that fallback fired; the returned response is the
// final one and its body belongs to the caller.
func issueGET(ctx context.Context, hc *http.Client, url, accept, tok string) (resp *http.Response, tokenRejected bool, err error) {
	resp, err = doGET(ctx, hc, url, accept, tok)
	if err != nil {
		return nil, false, err
	}
	if tok == "" || resp.StatusCode != http.StatusUnauthorized {
		return resp, false, nil
	}
	resp.Body.Close()
	resp, err = doGET(ctx, hc, url, accept, "")
	if err != nil {
		return nil, true, err
	}
	return resp, true, nil
}

// doGET issues one request. Release reads stay on the API host; asset
// downloads redirect to a signed host, and Go strips the Authorization
// header on the cross-host hop automatically (see Asset.Download).
func doGET(ctx context.Context, hc *http.Client, url, accept, tok string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "onegw/"+Version())
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return hc.Do(req)
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
	hc := &http.Client{Timeout: 10 * time.Minute}
	// Same credential discipline as release reads: a rejected token must
	// not block the download for a public repo.
	resp, _, err := issueGET(ctx, hc, dl, "application/octet-stream", token())
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
