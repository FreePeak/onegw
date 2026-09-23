package provider

import (
	"net/http"
	"testing"
)

// The shared pool's rotation: bulk URLs cycle per request, bypassed
// hosts go direct, and an empty pool never builds a client.
func TestProxySelectorRoundRobin(t *testing.T) {
	sel := newProxySelector(ProxyPool{URLs: []string{"http://p1:8080", "http://p2:8080", "http://p3:8080"}})
	if sel == nil {
		t.Fatal("selector must build for a non-empty pool")
	}
	req, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	var got []string
	for range 6 {
		u, err := sel.proxyFunc(req)
		if err != nil || u == nil {
			t.Fatalf("proxyFunc = %v, %v; want a pool URL", u, err)
		}
		got = append(got, u.Host)
	}
	for i, want := range []string{"p1:8080", "p2:8080", "p3:8080", "p1:8080", "p2:8080", "p3:8080"} {
		if got[i] != want {
			t.Fatalf("call %d = %s, want %s (full sequence %v)", i, got[i], want, got)
		}
	}
}

func TestProxySelectorRandomStaysInPool(t *testing.T) {
	sel := newProxySelector(ProxyPool{URLs: []string{"http://p1:8080", "http://p2:8080"}, Rotation: "random"})
	req, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	seen := map[string]bool{}
	for range 50 {
		u, err := sel.proxyFunc(req)
		if err != nil || u == nil {
			t.Fatalf("proxyFunc = %v, %v; want a pool URL", u, err)
		}
		if u.Host != "p1:8080" && u.Host != "p2:8080" {
			t.Fatalf("random pick %s outside the pool", u.Host)
		}
		seen[u.Host] = true
	}
	if len(seen) != 2 {
		t.Fatalf("50 random picks stayed on one proxy (%v); want both used", seen)
	}
}

func TestProxySelectorOrderPinsFirst(t *testing.T) {
	sel := newProxySelector(ProxyPool{URLs: []string{"http://p1:8080", "http://p2:8080"}, Rotation: "order"})
	req, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	for range 3 {
		u, err := sel.proxyFunc(req)
		if err != nil || u == nil || u.Host != "p1:8080" {
			t.Fatalf("order strategy = %v, %v; want pinned p1:8080", u, err)
		}
	}
}

func TestProxyBypassedHostsGoDirect(t *testing.T) {
	sel := newProxySelector(ProxyPool{
		URLs:    []string{"http://p1:8080"},
		NoProxy: "localhost,127.0.0.1,.internal",
	})
	for host, want := range map[string]bool{
		"localhost":         true,
		"127.0.0.1":         true,
		"svc.internal":      true,
		"api.example.com":   false,
		"internal.evil.com": false,
	} {
		req, _ := http.NewRequest("POST", "https://"+host+"/v1/chat", nil)
		u, err := sel.proxyFunc(req)
		if err != nil {
			t.Fatalf("host %s: unexpected error %v", host, err)
		}
		if want && u != nil {
			t.Errorf("host %s: got proxy %v, want direct (nil)", host, u)
		}
		if !want && (u == nil || u.Host != "p1:8080") {
			t.Errorf("host %s: got %v, want p1:8080", host, u)
		}
	}
}

func TestProxyEmptyPoolBuildsNothing(t *testing.T) {
	if sel := newProxySelector(ProxyPool{}); sel != nil {
		t.Fatal("empty pool must yield no selector (direct requests)")
	}
}

// An opted-in Def resolves its own client with the pool on the
// transport; a non-opted-in Def keeps the shared direct client.
func TestProxyDefClientWiring(t *testing.T) {
	pool := ProxyPool{URLs: []string{"http://p1:8080"}}
	d := &Def{Name: "p", Kind: KindOpenAI, Accounts: []Account{{Name: "a", APIKey: "k"}}}
	d.SetProxyPool(pool)
	req, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	got, err := d.httpClient().Transport.(*http.Transport).Proxy(req)
	if err != nil || got == nil || got.Host != "p1:8080" {
		t.Fatalf("opted-in transport proxy = %v, %v; want p1:8080", got, err)
	}

	plain := &Def{Name: "q", Kind: KindOpenAI, Accounts: []Account{{Name: "a", APIKey: "k"}}}
	if plain.httpClient() != client {
		t.Fatal("non-opted-in Def must keep the shared direct client")
	}
}

// Credentials embedded in pool URLs survive onto the wire (http.ProxyURL
// shape): the transport sends Proxy-Authorization, not the gateway.
func TestProxyURLCarriesCredentials(t *testing.T) {
	sel := newProxySelector(ProxyPool{URLs: []string{"http://user:pass@p1:8080"}})
	req, _ := http.NewRequest("POST", "https://api.example.com/v1/chat", nil)
	u, err := sel.proxyFunc(req)
	if err != nil || u == nil {
		t.Fatalf("proxyFunc = %v, %v", u, err)
	}
	if u.User == nil || u.User.Username() != "user" {
		t.Fatalf("proxy URL lost credentials: %v", u)
	}
}
