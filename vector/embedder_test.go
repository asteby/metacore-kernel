package vector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// testServer helpers
// ---------------------------------------------------------------------------

type testServer struct {
	URL    string
	server *httptest.Server
}

func (ts *testServer) close() {
	if ts.server != nil {
		ts.server.Close()
	}
}

func newTestServer(fn http.HandlerFunc) *testServer {
	ts := &testServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(fn))
	ts.URL = ts.server.URL
	return ts
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGenerateEmbedding_HappyPath(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["model"] != "bge-m3" {
			t.Errorf("model = %v, want bge-m3", req["model"])
		}
		if req["prompt"] != "hello world" {
			t.Errorf("prompt = %v, want hello world", req["prompt"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float32{0.1, 0.2, 0.3},
		})
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{
		BaseURL: ts.URL,
		Model:   "bge-m3",
		Timeout: 5 * time.Second,
	})
	vec, err := emb.GenerateEmbedding("hello world")
	if err != nil {
		t.Fatalf("GenerateEmbedding: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("len(vec) = %d, want 3", len(vec))
	}
	if vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Fatalf("vec = %v, want [0.1 0.2 0.3]", vec)
	}
}

func TestGenerateEmbedding_Non200(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("oops"))
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{BaseURL: ts.URL})
	_, err := emb.GenerateEmbedding("test")
	if err == nil {
		t.Fatal("expected error for non-200")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain 400: %v", err)
	}
}

func TestGenerateEmbedding_EmptyEmbedding(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{}})
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{BaseURL: ts.URL})
	_, err := emb.GenerateEmbedding("test")
	if err == nil {
		t.Fatal("expected error for empty embedding")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty': %v", err)
	}
}

func TestGenerateEmbedding_MalformedJSON(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{BaseURL: ts.URL})
	_, err := emb.GenerateEmbedding("test")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention 'decode': %v", err)
	}
}

func TestGenerateEmbedding_8000CharTruncation(t *testing.T) {
	var received string
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float32{0.1},
		})
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{BaseURL: ts.URL})
	longText := strings.Repeat("a", 9000)
	_, err := emb.GenerateEmbedding(longText)
	if err != nil {
		t.Fatalf("GenerateEmbedding: %v", err)
	}
	// Check the prompt in the JSON that was sent
	var req map[string]any
	json.Unmarshal([]byte(received), &req)
	prompt := req["prompt"].(string)
	if len(prompt) > 8000 {
		t.Errorf("prompt should be truncated to 8000 chars, got %d", len(prompt))
	}
}

func TestGenerateBatchEmbeddings_PartialFailure(t *testing.T) {
	var calls int
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("fail"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"embedding": []float32{float32(calls) * 0.1},
		})
	})
	defer ts.close()

	emb := NewRemoteEmbedder(RemoteEmbedderConfig{BaseURL: ts.URL})
	out, err := emb.GenerateBatchEmbeddings([]string{"ok", "fail", "ok"})
	if err != nil {
		t.Fatalf("GenerateBatchEmbeddings returned error: %v (should only log)", err)
	}
	if len(out) != 2 {
		t.Errorf("len(out) = %d, want 2", len(out))
	}
}

func TestNewRemoteEmbedder_Defaults(t *testing.T) {
	emb := NewRemoteEmbedder(RemoteEmbedderConfig{})
	if emb.baseURL != "https://ia.asteby.com" {
		t.Errorf("baseURL = %q, want https://ia.asteby.com", emb.baseURL)
	}
	if emb.model != "bge-m3" {
		t.Errorf("model = %q, want bge-m3", emb.model)
	}
	if emb.client == nil {
		t.Error("client should not be nil")
	}
}

func TestNewEnvRemoteEmbedder_EmptyEnv(t *testing.T) {
	origURL := os.Getenv("BGE_EMBEDDING_URL")
	origModel := os.Getenv("BGE_EMBEDDING_MODEL")
	defer func() {
		os.Setenv("BGE_EMBEDDING_URL", origURL)
		os.Setenv("BGE_EMBEDDING_MODEL", origModel)
	}()
	os.Setenv("BGE_EMBEDDING_URL", "")
	os.Setenv("BGE_EMBEDDING_MODEL", "")

	emb := NewEnvRemoteEmbedder()
	if emb.baseURL != "https://ia.asteby.com" {
		t.Errorf("baseURL = %q, want default", emb.baseURL)
	}
	if emb.model != "bge-m3" {
		t.Errorf("model = %q, want default", emb.model)
	}
}

func TestNewEnvRemoteEmbedder_FromEnv(t *testing.T) {
	origURL := os.Getenv("BGE_EMBEDDING_URL")
	origModel := os.Getenv("BGE_EMBEDDING_MODEL")
	defer func() {
		os.Setenv("BGE_EMBEDDING_URL", origURL)
		os.Setenv("BGE_EMBEDDING_MODEL", origModel)
	}()
	os.Setenv("BGE_EMBEDDING_URL", "http://custom:8080")
	os.Setenv("BGE_EMBEDDING_MODEL", "my-model")

	emb := NewEnvRemoteEmbedder()
	if emb.baseURL != "http://custom:8080" {
		t.Errorf("baseURL = %q, want http://custom:8080", emb.baseURL)
	}
	if emb.model != "my-model" {
		t.Errorf("model = %q, want my-model", emb.model)
	}
}
