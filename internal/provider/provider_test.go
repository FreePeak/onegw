package provider

import "testing"

func TestReplaceSwapsWholeSet(t *testing.T) {
	p := NewPool()
	p.Set(&Def{Name: "a", Kind: KindOpenAI, BaseURL: "https://a.example", Accounts: []Account{{Name: "default", APIKey: "k"}}})
	p.Set(&Def{Name: "b", Kind: KindOpenAI, BaseURL: "https://b.old", Accounts: []Account{{Name: "default", APIKey: "k"}}})

	p.Replace([]*Def{
		{Name: "b", Kind: KindOpenAI, BaseURL: "https://b.new", Accounts: []Account{{Name: "default", APIKey: "k"}}},
		{Name: "c", Kind: KindOpenAI, BaseURL: "https://c.example", Accounts: []Account{{Name: "default", APIKey: "k"}}},
		{Name: "c", Kind: KindOpenAI, BaseURL: "https://c.dup", Accounts: []Account{{Name: "default", APIKey: "k"}}},
	})

	if _, ok := p.Get("a"); ok {
		t.Fatal("provider a should be gone after Replace")
	}
	b, ok := p.Get("b")
	if !ok || b.BaseURL != "https://b.new" {
		t.Fatalf("b should resolve to the new definition, got %+v ok=%v", b, ok)
	}
	if _, ok := p.Get("c"); !ok {
		t.Fatal("c should be registered")
	}
	want := []string{"b", "c"}
	got := p.Names()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v (duplicates must dedup)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestReplaceInflightDefKeepsServing(t *testing.T) {
	p := NewPool()
	old := &Def{Name: "a", Kind: KindOpenAI, BaseURL: "https://a.example", Accounts: []Account{{Name: "default", APIKey: "k"}}}
	p.Set(old)
	resolved, _ := p.Get("a")

	p.Replace([]*Def{{Name: "a", Kind: KindOpenAI, BaseURL: "https://a.v2", Accounts: []Account{{Name: "default", APIKey: "k"}}}})

	if resolved.BaseURL != "https://a.example" {
		t.Fatal("an already-resolved *Def must stay usable for in-flight requests")
	}
	fresh, _ := p.Get("a")
	if fresh.BaseURL != "https://a.v2" {
		t.Fatalf("new resolutions must see the replacement, got %s", fresh.BaseURL)
	}
}
