package enrich

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrBatchUnsupported is returned by EnrichBatch when the active LLM provider
// does not implement BatchLLMProvider (e.g. Ollama / OpenAI / Google here). The
// caller falls back to the synchronous per-engram path.
var ErrBatchUnsupported = errors.New("enrich: provider does not support the batch API")

// Message Batches API support for Anthropic enrichment.
//
// The synchronous Complete() path (anthropic.go) is real-time and bills at full
// price — right for interactive, on-demand enrichment. The RETROACTIVE/background
// sweep, by contrast, enriches a backlog of engrams where a few minutes of latency
// is fine. That is the textbook case for the Message Batches API, which is **50%
// cheaper on both input and output**.
//
// This file exposes the three batch primitives (submit / poll / fetch) as methods
// on AnthropicLLMProvider, plus the BatchLLMProvider interface the worker
// type-asserts for. The worker owns the async lifecycle (collect → submit →
// persist batch id → poll → write results back); these are just the HTTP calls.
//
// Endpoints (GA, no beta header required):
//   POST {baseURL}/v1/messages/batches          submit
//   GET  {baseURL}/v1/messages/batches/{id}      poll status
//   GET  {results_url}                           fetch results (JSONL)

// BatchItem is one enrichment request in a batch. CustomID maps the result back
// to its engram (use the engram id string) — batch results are unordered.
type BatchItem struct {
	CustomID string
	System   string
	User     string
}

// BatchResult is the outcome for one BatchItem. Err is non-empty when the
// request did not succeed (errored / expired / canceled).
type BatchResult struct {
	Text string
	Err  string
}

// BatchStatus is a batch's processing state from a poll.
type BatchStatus struct {
	ID         string
	Status     string // "in_progress" | "canceling" | "ended"
	Ended      bool
	ResultsURL string // populated once Ended
	Processing int
	Succeeded  int
	Errored    int
	Canceled   int
	Expired    int
}

// BatchLLMProvider is implemented by providers supporting the async Message
// Batches API. The enrichment worker type-asserts the active provider for this
// and falls back to the synchronous Complete() path when it is not implemented
// (Ollama / OpenAI / Google here).
type BatchLLMProvider interface {
	SubmitBatch(ctx context.Context, items []BatchItem) (batchID string, err error)
	PollBatch(ctx context.Context, batchID string) (BatchStatus, error)
	FetchBatchResults(ctx context.Context, resultsURL string) (map[string]BatchResult, error)
}

// Compile-time assertion that AnthropicLLMProvider satisfies BatchLLMProvider.
var _ BatchLLMProvider = (*AnthropicLLMProvider)(nil)

// ── wire types ─────────────────────────────────────────────────────────────

type anthropicBatchRequestItem struct {
	CustomID string                   `json:"custom_id"`
	Params   anthropicMessagesRequest `json:"params"`
}

type anthropicBatchSubmitBody struct {
	Requests []anthropicBatchRequestItem `json:"requests"`
}

type anthropicBatchMeta struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
	ResultsURL       string `json:"results_url"`
	RequestCounts    struct {
		Processing int `json:"processing"`
		Succeeded  int `json:"succeeded"`
		Errored    int `json:"errored"`
		Canceled   int `json:"canceled"`
		Expired    int `json:"expired"`
	} `json:"request_counts"`
}

type anthropicBatchResultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string                     `json:"type"` // succeeded|errored|canceled|expired
		Message *anthropicMessagesResponse `json:"message"`
	} `json:"result"`
}

// ── primitives ─────────────────────────────────────────────────────────────

// SubmitBatch submits one batch of enrichment requests and returns its id. Each
// item reuses the same request shape as Complete(): the system prompt as a
// cacheable block (5-min ephemeral; no beta header needed) and the per-engram
// content as the user message.
func (p *AnthropicLLMProvider) SubmitBatch(ctx context.Context, items []BatchItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("SubmitBatch: no items")
	}

	reqs := make([]anthropicBatchRequestItem, 0, len(items))
	for _, it := range items {
		reqs = append(reqs, anthropicBatchRequestItem{
			CustomID: it.CustomID,
			Params: anthropicMessagesRequest{
				Model:     p.model,
				MaxTokens: p.maxTokens,
				System: []anthropicSystemBlock{
					{
						Type:         "text",
						Text:         it.System,
						CacheControl: &anthropicCacheControl{Type: "ephemeral"},
					},
				},
				Messages: []anthropicMessage{{Role: "user", Content: it.User}},
			},
		})
	}

	body, err := json.Marshal(anthropicBatchSubmitBody{Requests: reqs})
	if err != nil {
		return "", fmt.Errorf("marshal batch: %w", err)
	}

	var meta anthropicBatchMeta
	if err := p.doBatchJSON(ctx, "POST", p.baseURL+"/v1/messages/batches", body, &meta); err != nil {
		return "", err
	}
	if meta.ID == "" {
		return "", fmt.Errorf("batch submit returned empty id")
	}
	return meta.ID, nil
}

// PollBatch returns the current processing state of a batch.
func (p *AnthropicLLMProvider) PollBatch(ctx context.Context, batchID string) (BatchStatus, error) {
	var meta anthropicBatchMeta
	if err := p.doBatchJSON(ctx, "GET", p.baseURL+"/v1/messages/batches/"+batchID, nil, &meta); err != nil {
		return BatchStatus{}, err
	}
	return BatchStatus{
		ID:         meta.ID,
		Status:     meta.ProcessingStatus,
		Ended:      meta.ProcessingStatus == "ended",
		ResultsURL: meta.ResultsURL,
		Processing: meta.RequestCounts.Processing,
		Succeeded:  meta.RequestCounts.Succeeded,
		Errored:    meta.RequestCounts.Errored,
		Canceled:   meta.RequestCounts.Canceled,
		Expired:    meta.RequestCounts.Expired,
	}, nil
}

// FetchBatchResults streams the JSONL results file and returns a map keyed by
// CustomID. Non-succeeded results carry their type in BatchResult.Err so the
// caller can retry only the failures.
func (p *AnthropicLLMProvider) FetchBatchResults(ctx context.Context, resultsURL string) (map[string]BatchResult, error) {
	if resultsURL == "" {
		return nil, fmt.Errorf("FetchBatchResults: empty results url")
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", resultsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create results request: %w", err)
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch results: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("results fetch status %d: %s", resp.StatusCode, string(b))
	}

	out := make(map[string]BatchResult)
	scanner := bufio.NewScanner(resp.Body)
	// Batch result lines can be large (full message content); raise the limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rl anthropicBatchResultLine
		if err := json.Unmarshal(line, &rl); err != nil {
			return nil, fmt.Errorf("parse result line: %w", err)
		}
		if rl.Result.Type == "succeeded" && rl.Result.Message != nil {
			out[rl.CustomID] = BatchResult{Text: firstText(rl.Result.Message)}
		} else {
			out[rl.CustomID] = BatchResult{Err: rl.Result.Type}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan results: %w", err)
	}
	return out, nil
}

// doBatchJSON performs a JSON request against the batches endpoint and decodes
// the response into out. body may be nil for GETs.
func (p *AnthropicLLMProvider) doBatchJSON(ctx context.Context, method, url string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return fmt.Errorf("create %s %s: %w", method, url, err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s status %d: %s", method, url, resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", url, err)
	}
	return nil
}

// firstText returns the first text content block of a messages response.
func firstText(m *anthropicMessagesResponse) string {
	for _, b := range m.Content {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}
