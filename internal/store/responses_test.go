package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResponsesPutGetDeletePrune(t *testing.T) {
	s := openTest(t)
	old := ResponseRow{ID: "resp_old", Provider: "p1", Account: "a", Model: "m", APIKey: "pool",
		Created: time.Now().Add(-48 * time.Hour), Input: `[{"role":"user","content":"hi"}]`, Output: `[]`}
	cur := ResponseRow{ID: "resp_new", Provider: "p2", Account: "b", Model: "m", APIKey: "pool", PrevID: "resp_old"}
	for _, r := range []ResponseRow{old, cur} {
		if err := s.PutResponse(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.GetResponse("resp_new")
	if err != nil || !ok || got.Provider != "p2" || got.Account != "b" || got.PrevID != "resp_old" || got.APIKey != "pool" {
		t.Fatalf("get resp_new = %+v ok=%v err=%v", got, ok, err)
	}
	if got, ok, _ := s.GetResponse("resp_old"); !ok || got.Input != old.Input {
		t.Fatalf("history input not kept: %+v", got)
	}
	if _, ok, _ := s.GetResponse("resp_missing"); ok {
		t.Fatal("unknown id must report ok=false")
	}
	n, err := s.PruneResponses(time.Now().Add(-24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1 row", n, err)
	}
	if _, ok, _ := s.GetResponse("resp_old"); ok {
		t.Fatal("pruned row still present")
	}
	if err := s.DeleteResponse("resp_new"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetResponse("resp_new"); ok {
		t.Fatal("deleted row still present")
	}
}

// Rows survive closing and reopening the database (process restart).
func TestResponsesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutResponse(ResponseRow{ID: "resp_1", Provider: "p1", Account: "a", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, ok, err := s.GetResponse("resp_1"); err != nil || !ok || r.Provider != "p1" {
		t.Fatalf("after reopen: %+v ok=%v err=%v", r, ok, err)
	}
}
