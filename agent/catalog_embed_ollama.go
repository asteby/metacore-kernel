package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaEmbedder implements Embedder over Ollama's /api/embed endpoint (for
// example bge-m3 on the org's inference server). Batches are sent in one
// request.
type OllamaEmbedder struct {
	// BaseURL is the Ollama server ("https://ia.example.com").
	BaseURL string
	// Model is the embedding model (default "bge-m3").
	Model string
	// Client is the HTTP client (default: 10s timeout).
	Client *http.Client
	// Header optionally adds request headers (auth tokens).
	Header http.Header
}

// Embed implements Embedder.
func (e OllamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	model := e.Model
	if model == "" {
		model = "bge-m3"
	}
	body, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.BaseURL, "/")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range e.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ollama embed: %s: %s", resp.Status, ClipRunes(string(raw), 200))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, errEmbedCount
	}
	return out.Embeddings, nil
}
