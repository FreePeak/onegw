package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want Semver
		ok   bool
	}{
		{"v0.2.1", Semver{0, 2, 1, ""}, true},
		{"0.10.3", Semver{0, 10, 3, ""}, true},
		{"v1.0.0-rc.1", Semver{1, 0, 0, "rc.1"}, true},
		{"(devel)", Semver{}, false},
		{"abc123", Semver{}, false},
		{"v1.2", Semver{}, false},
		{"v1.2.3.4", Semver{}, false},
		{"", Semver{}, false},
		{"v999999999999.1.1", Semver{}, false}, // guards overflow-ish parses
	}
	for i, c := range cases {
		got, ok := ParseSemver(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("case %d: ParseSemver(%q) = %v,%v want %v,%v", i, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	asc := []string{"(devel)", "v0.0.1", "v0.0.2", "v0.1.0", "v0.9.9", "v1.0.0-rc.1", "v1.0.0-rc.2", "v1.0.0", "v1.0.1", "v2.0.0"}
	for i := 1; i < len(asc); i++ {
		if c := Compare(asc[i-1], asc[i]); c >= 0 {
			t.Errorf("Compare(%s, %s) = %d, want < 0", asc[i-1], asc[i], c)
		}
		if c := Compare(asc[i], asc[i-1]); c <= 0 {
			t.Errorf("Compare(%s, %s) = %d, want > 0", asc[i], asc[i-1], c)
		}
	}
	for _, v := range asc {
		if c := Compare(v, v); c != 0 {
			t.Errorf("Compare(%s, %s) = %d, want 0", v, v, c)
		}
	}
}

// fakeHub spins a fake GitHub releases API: GET /repos/r/releases/latest
// serves relJSON; asset downloads are served under /dl/<name>.
func fakeHub(t *testing.T, relJSON string, assets map[string][]byte) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(relJSON))
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if b, ok := assets[name]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &Client{Base: srv.URL, Repo: "r", Token: "test-token"}
}

func TestLatestAndSelectAsset(t *testing.T) {
	assetName := "onegw-" + runtime.GOOS + "-" + runtime.GOARCH
	relJSON := `{"tag_name":"v9.9.9","assets":[{"name":"` + assetName + `","url":"https://example.test/dl/x","browser_download_url":"x","size":3}]}`
	_, c := fakeHub(t, relJSON, nil)
	rel, err := c.Latest(context.Background())
	if rel.Tag != "v9.9.9" {
		t.Fatalf("tag = %s", rel.Tag)
	}
	a, err := rel.SelectAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil || a.Name != assetName {
		t.Fatalf("SelectAsset: %v, %v", a, err)
	}
	if _, err := rel.SelectAsset("plan9", "mips"); err == nil {
		t.Fatal("missing asset must error")
	}
}

func TestLatestBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Repo: "r", Token: "wrong"}
	_, err := c.Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
}

func TestDownloadDigestVerification(t *testing.T) {
	body := []byte("BIN")
	good := fmt.Sprintf("%x", sha256.Sum256(body))
	dir := t.TempDir()

	// Correct digest: download succeeds.
	srv, _ := fakeHub(t, "", map[string][]byte{"a": body})
	rel := &Release{Tag: "v1", Assets: []Asset{{Name: "a", APIURL: srv.URL + "/dl/a", Size: 3, Digest: "sha256:" + good}}}
	dst := filepath.Join(dir, "onegw.new")
	if err := rel.Assets[0].Download(context.Background(), dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "BIN" {
		t.Fatalf("content = %q", got)
	}

	// Corrupt digest: download must fail and remove the partial file.
	rel.Assets[0].Digest = "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte("other")))
	if err := rel.Assets[0].Download(context.Background(), dst); err == nil {
		t.Fatal("digest mismatch must fail")
	}
	// Corrupt attempt must not disturb the previously verified file.
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("partial temp file must be removed, stat err = %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "BIN" {
		t.Fatalf("good file must survive a failed re-download, got %q", got)
	}
}

func TestInContainerEnv(t *testing.T) {
	t.Setenv("ONEGW_IN_CONTAINER", "1")
	if !InContainer() {
		t.Fatal("env flag must force container mode")
	}
}
