package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OpenAIEmbedder implements cortexdb's Embedder interface (Embed / EmbedBatch /
// Dim) against any OpenAI-compatible /embeddings endpoint. Cloud only.
type OpenAIEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dim     int
	client  *http.Client
}

// NewOpenAIEmbedder builds an embedder. baseURL should include the API root
// (e.g. https://api.openai.com/v1). dim is the embedding dimension of the model.
func NewOpenAIEmbedder(baseURL, apiKey, model string, dim int) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		dim:     dim,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// Dim returns the embedding dimension.
func (e *OpenAIEmbedder) Dim() int { return e.dim }

// Embed embeds a single text.
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("embedder returned no vectors")
	}
	return vs[0], nil
}

// maxRequestBatch caps how many texts go in one /embeddings request. Some
// providers (e.g. DashScope text-embedding-v4) reject batches larger than ~10,
// while ollama accepts any size; sub-batching keeps both happy. Callers may pass
// arbitrarily large batches and this splits them transparently.
const maxRequestBatch = 10

// EmbedBatch embeds multiple texts, splitting into provider-safe sub-batches and
// preserving input order.
func (e *OpenAIEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) <= maxRequestBatch {
		return e.embedOnce(ctx, texts)
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxRequestBatch {
		end := start + maxRequestBatch
		if end > len(texts) {
			end = len(texts)
		}
		vs, err := e.embedOnce(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

// embedOnce sends a single /embeddings request for up to maxRequestBatch texts.
func (e *OpenAIEmbedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]interface{}{"model": e.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("embeddings api %d: %s", resp.StatusCode, buf.String())
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}
