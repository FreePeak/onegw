package config

import (
	"testing"
)

const baseCfg = `
[server]
data_dir = "memory"
[[providers]]
name = "p"
kind = "openai"
api_key = "k"
models = ["m1"]
`

func TestLoadSaverOutputSections(t *testing.T) {
	cfg, err := Load(writeCfg(t, baseCfg+`
[saver]
enabled = true
[[saver.inject]]
mode = "terse"
models = ["gpt-*"]
[[saver.inject]]
mode = "custom"
text = "five words max"
[saver.external]
enabled = true
url = "http://127.0.0.1:8819"
timeout_ms = 900
min_bytes = 1024
fail_open = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Saver.Inject) != 2 {
		t.Fatalf("want 2 inject rules, got %d", len(cfg.Saver.Inject))
	}
	if cfg.Saver.Inject[0].Mode != "terse" || cfg.Saver.Inject[0].Models[0] != "gpt-*" {
		t.Fatalf("inject[0] wrong: %+v", cfg.Saver.Inject[0])
	}
	if cfg.Saver.Inject[1].Mode != "custom" || cfg.Saver.Inject[1].Text != "five words max" {
		t.Fatalf("inject[1] wrong: %+v", cfg.Saver.Inject[1])
	}
	e := cfg.Saver.External
	if !e.Enabled || e.URL != "http://127.0.0.1:8819" || e.TimeoutMS != 900 || e.MinBytes != 1024 {
		t.Fatalf("external wrong: %+v", e)
	}
	if e.FailOpen == nil || *e.FailOpen {
		t.Fatalf("fail_open=false not decoded: %+v", e.FailOpen)
	}
}

func TestValidateSaverInjectMode(t *testing.T) {
	for _, body := range []string{
		baseCfg + "\n[[saver.inject]]\nmode = \"wat\"\n",
		baseCfg + "\n[[saver.inject]]\n",
		baseCfg + "\n[[saver.inject]]\nmode = \"custom\"\n",
		baseCfg + "\n[saver.external]\nenabled = true\n",
	} {
		cfg, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("expected rejection, got %+v", cfg.Saver)
		}
	}
}

func TestValidateSaverExternalDefaults(t *testing.T) {
	// enabled without fail_open: default true (nil pointer accepted).
	cfg, err := Load(writeCfg(t, baseCfg+`
[saver.external]
enabled = true
url = "http://127.0.0.1:8819"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Saver.External.FailOpen != nil {
		t.Fatalf("unset fail_open must stay nil (defaults apply at runtime)")
	}
}
