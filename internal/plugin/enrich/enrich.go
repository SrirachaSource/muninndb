package enrich

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine/circuit"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/llmstats"
	"github.com/scrypster/muninndb/internal/storage"
)

// LLMProvider is the internal interface for LLM HTTP clients.
type LLMProvider interface {
	Name() string
	Init(ctx context.Context, cfg LLMProviderConfig) error
	Complete(ctx context.Context, system, user string) (string, error)
	Close() error
}

// LLMProviderConfig is the resolved configuration for an LLM provider.
type LLMProviderConfig struct {
	BaseURL     string  // "http://localhost:11434" or "https://api.openai.com"
	Model       string  // "llama3.2" or "gpt-4o-mini" or "claude-haiku"
	APIKey      string  // empty for Ollama, required for cloud providers
	MaxTokens   int     // max response tokens (default: 1024)
	Temperature float32 // 0.0 for deterministic extraction (default: 0.0)
}

// defaultBreaker thresholds: 5 consecutive failures open the circuit;
// it probes again after 30 s.
const (
	breakerMaxFails   = 5
	breakerResetAfter = 30 * time.Second
)

// EnrichService implements plugin.EnrichPlugin.
type EnrichService struct {
	provider  LLMProvider
	pipeline  *EnrichmentPipeline
	limiter   *TokenBucketLimiter
	cfg       plugin.PluginConfig
	enrichCfg *config.PluginConfig
	name      string
	provCfg   *plugin.ProviderConfig
	mu        sync.Mutex
	closed    bool

	// breaker guards all LLM calls. It is constructed at service creation time
	// so it is always non-nil. OnStateChange may be wired after construction
	// (before concurrent use) to emit logs and update the plugin registry.
	breaker *circuit.Breaker
}

// EnrichService satisfies the batch-capable enrich interface (used by the
// retroactive sweep when the provider + config opt into batching).
var _ plugin.BatchEnrichPlugin = (*EnrichService)(nil)

// NewEnrichService creates an EnrichService from a provider URL.
func NewEnrichService(providerURL string) (*EnrichService, error) {
	provCfg, err := plugin.ParseProviderURL(providerURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse provider URL: %w", err)
	}

	var prov LLMProvider
	switch provCfg.Scheme {
	case plugin.SchemeOllama:
		prov = NewOllamaLLMProvider()
	case plugin.SchemeOpenAI:
		prov = NewOpenAILLMProvider()
	case plugin.SchemeAnthropic:
		prov = NewAnthropicLLMProvider()
	case plugin.SchemeGoogle:
		prov = NewGoogleLLMProvider()
	default:
		return nil, fmt.Errorf("unsupported enrich provider scheme: %q", provCfg.Scheme)
	}

	name := fmt.Sprintf("enrich-%s", provCfg.Scheme)

	es := &EnrichService{
		provider: prov,
		provCfg:  provCfg,
		name:     name,
		breaker:  circuit.New(breakerMaxFails, breakerResetAfter),
	}

	return es, nil
}

// Name returns the plugin identifier.
func (s *EnrichService) Name() string {
	return s.name
}

// Tier returns the plugin tier.
func (s *EnrichService) Tier() plugin.PluginTier {
	return plugin.TierEnrich
}

// Init validates configuration and external connectivity.
func (s *EnrichService) Init(ctx context.Context, cfg plugin.PluginConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg = cfg

	provHTTPCfg := LLMProviderConfig{
		BaseURL:     s.provCfg.BaseURL,
		Model:       s.provCfg.Model,
		APIKey:      cfg.APIKey,
		MaxTokens:   1024,
		Temperature: 0.0,
	}

	slog.Info("initializing enrich provider",
		"name", s.name,
		"base_url", provHTTPCfg.BaseURL,
		"model", provHTTPCfg.Model,
	)

	// Initialize provider and validate connectivity
	if err := s.provider.Init(ctx, provHTTPCfg); err != nil {
		return fmt.Errorf("provider init failed: %w", err)
	}

	slog.Info("enrich provider initialized",
		"name", s.name,
	)

	// Set up rate limiter based on provider
	s.limiter = s.createRateLimiter(s.provCfg.Scheme)

	// Set up pipeline
	s.pipeline = NewPipeline(s.provider, s.limiter)
	if s.enrichCfg != nil {
		s.pipeline.SetConfig(s.enrichCfg)
	}

	return nil
}

// SetEnrichConfig applies server-level enrichment configuration (per-stage flags, mode).
// Must be called before Init, or the pipeline will use defaults.
func (s *EnrichService) SetEnrichConfig(cfg *config.PluginConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrichCfg = cfg
	if s.pipeline != nil {
		s.pipeline.SetConfig(cfg)
	}
}

// SetBreakerStateChangeHook wires a callback on the internal circuit breaker so
// that state transitions emit structured log lines and update the plugin registry.
// It must be called once, before concurrent use (i.e. before Start/Init).
//
// When the circuit opens, the callback emits slog.Warn and calls registry.SetUnhealthy.
// When the circuit recovers (→ Closed), it emits slog.Info and calls registry.SetHealthy.
func (s *EnrichService) SetBreakerStateChangeHook(registry interface {
	SetHealthy(name string, healthy bool)
	SetUnhealthy(name string, err error)
}) {
	pluginName := s.name
	providerName := string(s.provCfg.Scheme)
	s.breaker.OnStateChange = func(ev circuit.StateChangeEvent) {
		switch ev.To {
		case circuit.StateOpen:
			slog.Warn("enrich: circuit breaker opened — LLM provider unhealthy",
				"plugin", pluginName,
				"provider", providerName,
				"failure_count", ev.FailureCount,
				"state", "open",
			)
			registry.SetUnhealthy(pluginName, fmt.Errorf("circuit breaker open after %d consecutive failures", ev.FailureCount))
		case circuit.StateClosed:
			slog.Info("enrich: circuit breaker recovered — LLM provider healthy",
				"plugin", pluginName,
				"provider", providerName,
				"failure_count", ev.FailureCount,
				"state", "closed",
				"outage_duration", ev.OutageDuration.Round(time.Second).String(),
			)
			registry.SetHealthy(pluginName, true)
		}
	}
}

// Enrich processes one engram and returns enrichment data.
// The call is gated by the internal circuit breaker: if the LLM provider has
// been failing consecutively, ErrOpen is returned immediately without hitting
// the network. If the breaker is nil (e.g. when constructing EnrichService
// directly in tests), the pipeline is called without circuit-breaker gating.
func (s *EnrichService) Enrich(ctx context.Context, eng *storage.Engram) (*plugin.EnrichmentResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("enrich service is closed")
	}
	s.mu.Unlock()

	if s.pipeline == nil {
		return nil, fmt.Errorf("enrich service not initialized")
	}

	// Fast path: no circuit breaker (test construction or future embedded use).
	if s.breaker == nil {
		return s.pipeline.Run(ctx, eng)
	}

	var result *plugin.EnrichmentResult
	err := s.breaker.Do(func() error {
		var runErr error
		result, runErr = s.pipeline.Run(ctx, eng)
		return runErr
	})
	return result, err
}

// EnrichBatch enriches many engrams in one Anthropic Message Batch (50% cheaper),
// for the background retroactive sweep. Per-engram outcomes come back in the
// results/errs maps (keyed by engram-id string); a non-nil error means the whole
// batch failed (submit/poll/fetch) — the caller should retry the set. Returns
// ErrBatchUnsupported when the provider is not batch-capable (caller falls back
// to per-engram Enrich). The whole batch is gated by the circuit breaker as one
// operation, so a provider outage trips it the same as the synchronous path.
func (s *EnrichService) EnrichBatch(
	ctx context.Context, engs []*storage.Engram,
) (map[string]*plugin.EnrichmentResult, map[string]error, error) {
	s.mu.Lock()
	closed := s.closed
	pipeline := s.pipeline
	breaker := s.breaker
	s.mu.Unlock()

	if closed {
		return nil, nil, fmt.Errorf("enrich service is closed")
	}
	if pipeline == nil {
		return nil, nil, fmt.Errorf("enrich service not initialized")
	}

	if breaker == nil {
		return pipeline.EnrichBatch(ctx, engs)
	}

	var (
		results map[string]*plugin.EnrichmentResult
		errs    map[string]error
	)
	err := breaker.Do(func() error {
		var runErr error
		results, errs, runErr = pipeline.EnrichBatch(ctx, engs)
		return runErr
	})
	return results, errs, err
}

// LLMStats returns a point-in-time snapshot of LLM call metrics.
func (s *EnrichService) LLMStats() llmstats.Snapshot {
	s.mu.Lock()
	pipeline := s.pipeline
	s.mu.Unlock()
	if pipeline == nil {
		return llmstats.Snapshot{}
	}
	return pipeline.LLMStats()
}

// Close releases external connections.
func (s *EnrichService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true
	return s.provider.Close()
}

// createRateLimiter creates a rate limiter based on the provider scheme.
func (s *EnrichService) createRateLimiter(scheme plugin.ProviderScheme) *TokenBucketLimiter {
	switch scheme {
	case plugin.SchemeOllama:
		// No rate limiting for local Ollama
		return NewTokenBucketLimiter(1000.0, 1000.0)
	case plugin.SchemeOpenAI:
		// 10 requests per second for OpenAI (gpt-4o-mini)
		return NewTokenBucketLimiter(10.0, 10.0)
	case plugin.SchemeAnthropic:
		// 8 requests per second for Anthropic (claude-haiku)
		return NewTokenBucketLimiter(8.0, 8.0)
	case plugin.SchemeGoogle:
		// Gemini Flash paid tier: ~2000 RPM. Use 10 RPS as a conservative default.
		return NewTokenBucketLimiter(10.0, 10.0)
	default:
		// Default: 5 requests per second
		return NewTokenBucketLimiter(5.0, 5.0)
	}
}
