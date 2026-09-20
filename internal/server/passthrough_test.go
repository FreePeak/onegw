package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"onegw/internal/config"
)

// ptProviders builds a config: "passem" opts into all passthrough surfaces,
// "plain" does not (capability refusal target).
func ptProviders(t *testing.T, passemURL, plainURL string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = "memory"
	cfg.Auth.KeyList = []config.AuthKey{{Key: "test-key"}}
	cfg.Providers = []config.ProviderCfg{
		{
			Name: "passem", Kind: "openai", BaseURL: passemURL, APIKey: "up-key",
			Models:      []string{"emb-1", "tts-1", "stt-1"},
			Passthrough: []string{"embeddings", "stt", "tts"},
		},
		{Name: "plain", Kind: "openai", BaseURL: plainURL, APIKey: "up-key", Models: []string{"plain-emb"}},
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func ptSrv(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// multipartBody builds the multipart form a client would send to STT:
// model field first, then the file part. Returns body bytes and content type.
func multipartBody(t *testing.T, model string, audio []byte, filename, contentType string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestPassthroughEmbeddings(t *testing.T) {
	var gotPath, gotAuth, gotCT, gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &probe)
		gotModel = probe.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":7,"completion_tokens":0}}`))
	}))
	defer up.Close()

	srv := ptSrv(t, ptProviders(t, up.URL, ""))
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"model": "passem/emb-1", "input": "hello world"})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := do(t, h, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer up-key" {
		t.Errorf("upstream auth = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("upstream content-type = %q", gotCT)
	}
	if gotModel != "emb-1" {
		t.Errorf("upstream model = %q, want rewritten %q (client string must not leak)", gotModel, "emb-1")
	}
	if !strings.Contains(w.Body.String(), `"prompt_tokens":7`) {
		t.Errorf("client body missing sniffed usage: %s", w.Body.String())
	}
	reqs, input, _, _ := srv.cur().usage.Totals()
	if reqs != 1 || input != 7 {
		t.Errorf("usage totals = (reqs=%d, input=%d), want (1, 7)", reqs, input)
	}
}

func TestPassthroughSpeechStreamsBinary(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &probe)
		if probe.Model != "tts-1" {
			t.Errorf("upstream model = %q, want tts-1", probe.Model)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer up.Close()

	srv := ptSrv(t, ptProviders(t, up.URL, ""))

	body, _ := json.Marshal(map[string]any{"model": "passem/tts-1", "input": "hi", "voice": "alloy"})
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := do(t, srv.Handler(), r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("client content-type = %q, want audio/mpeg", ct)
	}
	if len(w.Body.Bytes()) != len(payload) || sha256.Sum256(w.Body.Bytes()) != sum {
		t.Errorf("audio payload not relayed byte-for-byte (got %d bytes)", len(w.Body.Bytes()))
	}
	reqs, _, _, _ := srv.cur().usage.Totals()
	if reqs != 1 {
		t.Errorf("speech request not counted: reqs=%d", reqs)
	}
}

func TestPassthroughTranscriptionsRelaysMultipartByteForByte(t *testing.T) {
	// Audio larger than the 8 KiB peek window proves streaming, not buffering.
	audio := make([]byte, 64<<10+123)
	if _, err := rand.Read(audio); err != nil {
		t.Fatal(err)
	}

	var gotCT string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello","usage":{"prompt_tokens":12,"completion_tokens":5}}`))
	}))
	defer up.Close()

	srv := ptSrv(t, ptProviders(t, up.URL, ""))

	buf, ct := multipartBody(t, "passem/stt-1", audio, "speech.webm", "audio/webm")
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	r.Header.Set("Authorization", "Bearer test-key")
	r.Header.Set("Content-Type", ct)
	orig := buf.Bytes()
	w := do(t, srv.Handler(), r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if gotCT != ct {
		t.Errorf("upstream content-type = %q, want boundary preserved %q", gotCT, ct)
	}
	// Byte-for-byte relay apart from the routed model rewrite
	// ("passem/stt-1" -> "stt-1"), mirroring the embeddings JSON path.
	want := bytes.Replace(orig, []byte("passem/stt-1"), []byte("stt-1"), 1)
	if !bytes.Equal(gotBody, want) {
		t.Errorf("multipart relay diverges beyond the model rewrite: got %d bytes, want %d", len(gotBody), len(want))
	}
	if bytes.Contains(gotBody, []byte("passem/stt-1")) {
		t.Error("provider/model string leaked upstream")
	}
	if !strings.Contains(w.Body.String(), `"prompt_tokens":12`) {
		t.Errorf("client body missing sniffed usage: %s", w.Body.String())
	}
	reqs, _, _, _ := srv.cur().usage.Totals()
	if reqs != 1 {
		t.Error("transcription request not counted")
	}
}

func TestPassthroughCapabilityAndAuthRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for refused requests")
	}))
	defer up.Close()

	srv := ptSrv(t, ptProviders(t, up.URL, up.URL))
	h := srv.Handler()

	// Provider without the passthrough marker.
	body, _ := json.Marshal(map[string]any{"model": "plain/plain-emb", "input": "x"})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := do(t, h, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "passthrough_not_supported") {
		t.Fatalf("capability refusal: status = %d body = %s", w.Code, w.Body.String())
	}

	// Unknown provider.
	body, _ = json.Marshal(map[string]any{"model": "nosuch/m", "input": "x"})
	r = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w = do(t, h, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_provider") {
		t.Fatalf("unknown provider: status = %d body = %s", w.Code, w.Body.String())
	}

	// Missing auth.
	r = httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"passem/emb-1","input":"x"}`))
	w = do(t, h, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status = %d", w.Code)
	}
}

func TestPassthroughUpstreamErrorRelayedVerbatim(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad upstream key"}}`))
	}))
	defer up.Close()

	srv := ptSrv(t, ptProviders(t, up.URL, ""))

	body, _ := json.Marshal(map[string]any{"model": "passem/emb-1", "input": "x"})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := do(t, srv.Handler(), r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad upstream key") {
		t.Errorf("upstream error body not relayed: %s", w.Body.String())
	}
}

func TestPassthroughConfigValidationRejectsUnknownCapability(t *testing.T) {
	cfg := ptProviders(t, "", "")
	cfg.Providers[0].Passthrough = []string{"bogus"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown passthrough capability") {
		t.Fatalf("Validate = %v, want unknown-passthrough error", err)
	}
	cfg.Providers[0].Passthrough = []string{"stt"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

func TestPassthroughComboFallsBackPastCapabilityRefusal(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	upOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[],"usage":{"prompt_tokens":2}}`))
	}))
	defer upOK.Close()

	cfg := ptProviders(t, upOK.URL, upOK.URL)
	cfg.Combos = []config.ComboCfg{{
		Name:    "emb-combo",
		Targets: []string{"plain/plain-emb", "passem/emb-1"},
	}}
	srv := ptSrv(t, cfg)

	body, _ := json.Marshal(map[string]any{"model": "emb-combo", "input": "x"})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := do(t, srv.Handler(), r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("passem calls = %d, want 1 (fallback after capability refusal)", calls)
	}
}
