package vector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// RemoteEmbedder calls a self-hosted or third-party embedding endpoint that
// speaks the Ollama-compatible `POST /api/embeddings` shape:
//
//	request:  { "model": "<model>", "prompt": "<text>" }
//	response: { "embedding": [<float32>...] }
//
// That contract matches Asteby's BGE-M3 server, llama.cpp's embeddings
// endpoint, ollama, and a handful of other open backends. Apps that ride a
// non-Ollama API (OpenAI, Cohere, Voyage…) can implement Embedder directly.
type RemoteEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
}

// RemoteEmbedderConfig configures a RemoteEmbedder. Empty fields fall back
// to the BGE-M3 defaults Asteby ships in production.
type RemoteEmbedderConfig struct {
	BaseURL string        // default: https://ia.asteby.com
	Model   string        // default: bge-m3
	Timeout time.Duration // default: 30s
}

// NewRemoteEmbedder builds a RemoteEmbedder. Pass an empty BaseURL/Model to
// take the BGE-M3 defaults.
func NewRemoteEmbedder(cfg RemoteEmbedderConfig) *RemoteEmbedder {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://ia.asteby.com"
	}
	if cfg.Model == "" {
		cfg.Model = "bge-m3"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &RemoteEmbedder{
		baseURL: cfg.BaseURL,
		model:   cfg.Model,
		client:  &http.Client{Timeout: cfg.Timeout},
	}
}

// NewEnvRemoteEmbedder is a convenience for the common case of reading the
// endpoint and model from env vars. Falls back to the BGE-M3 defaults when
// the variables are unset.
func NewEnvRemoteEmbedder() *RemoteEmbedder {
	return NewRemoteEmbedder(RemoteEmbedderConfig{
		BaseURL: os.Getenv("BGE_EMBEDDING_URL"),
		Model:   os.Getenv("BGE_EMBEDDING_MODEL"),
	})
}

// GenerateEmbedding embeds a single text into a vector. Texts longer than
// 8000 characters are truncated — BGE-M3's context limit; pass shorter
// chunks if you need full coverage.
func (e *RemoteEmbedder) GenerateEmbedding(text string) ([]float32, error) {
	if len(text) > 8000 {
		text = text[:8000]
	}
	payload := map[string]any{"model": e.model, "prompt": text}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("vector: marshal embedding payload: %w", err)
	}

	resp, err := e.client.Post(e.baseURL+"/api/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vector: call embedding API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vector: embedding API status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vector: decode embedding response: %w", err)
	}
	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("vector: empty embedding returned")
	}
	return result.Embedding, nil
}

// GenerateBatchEmbeddings embeds multiple texts. The default backend doesn't
// expose a batch endpoint, so this is a sequential loop with per-text error
// logging — failed embeddings are skipped, not propagated.
func (e *RemoteEmbedder) GenerateBatchEmbeddings(texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		emb, err := e.GenerateEmbedding(text)
		if err != nil {
			log.Printf("vector: embed batch item: %v", err)
			continue
		}
		out = append(out, emb)
	}
	return out, nil
}
