package claude

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelAcceptsSampling(t *testing.T) {
	cases := map[string]bool{
		"":                                  true,
		"auto":                              true,
		"gpt-4o":                            true,
		"deepseek-chat":                     true,
		"claude-3-5-sonnet-latest":          true,
		"claude-sonnet-4-5":                 true,
		"claude-sonnet-4-5-20250929":        true,
		"claude-haiku-4-5-20251001":         true,
		"claude-opus-5-5":                   false,
		"Claude-Opus-5-5":                   false,
		"claude-sonnet-5":                   false,
		"claude-fable-5-1":                  false,
		"claude-opus-5-5[1m]":               false,
		"anthropic/claude-sonnet-5":         false,
		"us.anthropic.claude-opus-5-5-v1:0": false,
		"claude-sonnet-6":                   false,
		"claude-20250101":                   true,
	}
	for model, want := range cases {
		if got := ModelAcceptsSampling(model); got != want {
			t.Errorf("ModelAcceptsSampling(%q) = %v; want %v", model, got, want)
		}
	}
}

func TestSetModelSamplingSupportOverridesFamilyRule(t *testing.T) {
	t.Cleanup(func() {
		samplingOverridesMu.Lock()
		delete(samplingOverrides, "mi-alias-claude5")
		delete(samplingOverrides, "claude-opus-5-5-legacy")
		samplingOverridesMu.Unlock()
	})
	SetModelSamplingSupport("mi-alias-claude5", false)
	SetModelSamplingSupport("Claude-Opus-5-5-Legacy", true)
	if ModelAcceptsSampling("mi-alias-claude5") {
		t.Fatal("el registro por capacidad debe marcar el alias como sin muestreo")
	}
	if !ModelAcceptsSampling("claude-opus-5-5-legacy") {
		t.Fatal("el registro por capacidad debe prevalecer sobre la regla por familia")
	}
}

func TestStripUnsupportedSampling(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","temperature":0,"top_p":0.9,"top_k":5,"max_tokens":10,"messages":[]}`)
	out, changed := StripUnsupportedSampling(body)
	if !changed {
		t.Fatal("se esperaba que retirara los parámetros de muestreo")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s debería haberse eliminado: %s", k, out)
		}
	}
	if got["max_tokens"].(float64) != 10 || got["model"] != "claude-opus-5-5" {
		t.Errorf("el resto del cuerpo debe conservarse: %s", out)
	}

	keep := []byte(`{"model":"claude-sonnet-4-5","temperature":0.2}`)
	if out, changed := StripUnsupportedSampling(keep); changed || string(out) != string(keep) {
		t.Errorf("Claude 4.x conserva temperature; got %s changed=%v", out, changed)
	}
	if _, changed := StripUnsupportedSampling([]byte(`not json`)); changed {
		t.Error("un cuerpo no JSON no debe tocarse")
	}
	if _, changed := StripUnsupportedSampling([]byte(`{"model":"claude-sonnet-5"}`)); changed {
		t.Error("sin parámetros de muestreo no hay cambio")
	}
}

func TestSamplingSanitizerRoundTrip(t *testing.T) {
	var seen map[string]any
	var seenLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seenLen = r.ContentLength
		if err := json.Unmarshal(raw, &seen); err != nil {
			t.Errorf("cuerpo inválido: %v", err)
		}
		if int64(len(raw)) != seenLen {
			t.Errorf("Content-Length %d no coincide con el cuerpo %d", seenLen, len(raw))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewSamplingSanitizer(nil)}
	post := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	post(`{"model":"claude-opus-5-5","temperature":0,"max_tokens":1}`)
	if _, ok := seen["temperature"]; ok {
		t.Fatalf("temperature no debe llegar a Claude 5.x: %v", seen)
	}
	post(`{"model":"claude-haiku-4-5-20251001","temperature":0.3,"max_tokens":1}`)
	if seen["temperature"] != 0.3 {
		t.Fatalf("Claude 4.x debe recibir temperature intacto: %v", seen)
	}
}
