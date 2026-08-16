package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/SigNoz/signoz/pkg/query-service/vigil"
)

// DefaultBaseURL is Featherless's OpenAI-compatible endpoint.
const DefaultBaseURL = "https://api.featherless.ai/v1"

// Config describes how to reach Featherless.
type Config struct {
	APIKey  string
	BaseURL string
	// Models maps each role to a model ID. There are deliberately no defaults:
	// model catalogues change, and a hardcoded ID that has since been retired
	// fails at runtime in production rather than at startup in review. An
	// operator who wants a role active must name the model.
	Models  map[Role]string
	Timeout time.Duration
	Retries int
}

// String redacts the key so an accidental %v or structured log of the whole
// config cannot leak it.
func (c Config) String() string {
	return fmt.Sprintf("llm.Config{BaseURL:%s, Models:%v, Timeout:%s, Retries:%d, APIKey:<redacted len=%d>}",
		c.BaseURL, c.Models, c.Timeout, c.Retries, len(c.APIKey))
}

// ConfigFromEnv reads provider configuration from the environment.
func ConfigFromEnv() Config {
	cfg := Config{
		APIKey:  vigil.Env("FEATHERLESS_API_KEY"),
		BaseURL: vigil.EnvOr("FEATHERLESS_BASE_URL", DefaultBaseURL),
		Models:  map[Role]string{},
		Timeout: 8 * time.Second,
		Retries: 2,
	}
	// Accept the bare FEATHERLESS_* spelling too; it is what the provider's own
	// docs use, so an operator copying from there should just work.
	if cfg.APIKey == "" {
		cfg.APIKey = osGetenv("FEATHERLESS_API_KEY")
	}
	for role, key := range map[Role]string{
		RoleFast:     "MODEL_FAST",
		RoleReasoner: "MODEL_REASONER",
		RoleReviewer: "MODEL_REVIEWER",
	} {
		if v := vigil.Env(key); v != "" {
			cfg.Models[role] = v
		}
	}
	if v := vigil.Env("LLM_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = time.Duration(n) * time.Second
		}
	}
	return cfg
}

// Featherless is an OpenAI-compatible chat-completions client.
type Featherless struct {
	cfg    Config
	logger *slog.Logger
	http   *http.Client
}

// NewFeatherless builds a client, or reports why it cannot.
//
// Missing credentials are an ordinary condition, not a failure of the
// deployment: the caller logs it and runs deterministic-only.
func NewFeatherless(logger *slog.Logger, cfg Config) (*Featherless, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("llm: no API key configured")
	}
	if len(cfg.Models) == 0 {
		return nil, errors.New("llm: no model IDs configured (set VIGIL_MODEL_FAST / _REASONER / _REVIEWER)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 8 * time.Second
	}
	return &Featherless{
		cfg:    cfg,
		logger: logger,
		// A dedicated client, never http.DefaultClient: DefaultClient has no
		// timeout, so a hung provider would pin a tool call open indefinitely.
		http: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

func (f *Featherless) Name() string { return "featherless" }

func (f *Featherless) Configured(role Role) bool { return f.cfg.Models[role] != "" }

// ConfiguredRoles lists the roles that have a model, for the status endpoint.
func (f *Featherless) ConfiguredRoles() []Role {
	out := make([]Role, 0, len(Roles))
	for _, r := range Roles {
		if f.Configured(r) {
			out = append(out, r)
		}
	}
	return out
}

// fallbackRole returns the next cheaper role to try, and whether one exists.
//
// Escalation is downward only. A reviewer outage falling back to the reasoner
// is a graceful degradation; the reverse would let a cheap model's transient
// failure silently invoke the most expensive one, turning an outage into a
// bill.
func fallbackRole(r Role) (Role, bool) {
	switch r {
	case RoleReviewer:
		return RoleReasoner, true
	case RoleReasoner:
		return RoleFast, true
	default:
		return "", false
	}
}

// Complete runs a request, retrying transient failures and falling back to a
// cheaper role's model if the requested one stays unavailable.
func (f *Featherless) Complete(ctx context.Context, req Request) (*Response, error) {
	role := req.Role
	for {
		model := f.cfg.Models[role]
		if model == "" {
			next, ok := fallbackRole(role)
			if !ok {
				return nil, ErrNoModel
			}
			role = next
			continue
		}

		resp, err := f.complete(ctx, req, role, model)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		next, ok := fallbackRole(role)
		if !ok {
			return nil, err
		}
		f.logger.WarnContext(ctx, "llm: falling back to cheaper model role",
			slog.String("from_role", string(role)),
			slog.String("to_role", string(next)),
			slog.String("error", err.Error()),
		)
		role = next
	}
}

// complete performs the request against one model, with bounded retries.
func (f *Featherless) complete(ctx context.Context, req Request, role Role, model string) (*Response, error) {
	const (
		baseDelay = 250 * time.Millisecond
		maxDelay  = 2 * time.Second
	)

	var lastErr error
	for attempt := 0; attempt <= f.cfg.Retries; attempt++ {
		if attempt > 0 {
			delay := baseDelay << (attempt - 1)
			if delay > maxDelay {
				delay = maxDelay
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, retryable, err := f.attempt(ctx, req, role, model)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable {
			// 4xx other than 429 will not become correct on a retry; a 401
			// retried three times is three log lines saying the key is wrong.
			return nil, err
		}
	}
	return nil, lastErr
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// attempt is one HTTP round trip. The bool reports whether a retry could help.
func (f *Featherless) attempt(ctx context.Context, req Request, role Role, model string) (*Response, bool, error) {
	body := chatRequest{
		Model:       model,
		Messages:    []chatMessage{},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.System != "" {
		body.Messages = append(body.Messages, chatMessage{Role: "system", Content: req.System})
	}
	body.Messages = append(body.Messages, chatMessage{Role: "user", Content: req.User})
	if req.JSONOnly {
		body.ResponseFormat = &struct {
			Type string `json:"type"`
		}{Type: "json_object"}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, false, err
	}

	attemptCtx, cancel := context.WithTimeout(ctx, f.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, f.cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+f.cfg.APIKey)

	start := time.Now()
	httpResp, err := f.http.Do(httpReq)
	if err != nil {
		return nil, true, err // network errors are worth one more try
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, true, err
	}
	latency := time.Since(start)

	if httpResp.StatusCode != http.StatusOK {
		retryable := httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500
		// Never include the response body verbatim in the error: provider error
		// payloads sometimes echo request headers.
		return nil, retryable, fmt.Errorf("llm: provider returned %d", httpResp.StatusCode)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false, fmt.Errorf("llm: unparseable provider response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, false, errors.New("llm: provider returned no choices")
	}

	served := parsed.Model
	if served == "" {
		served = model
	}
	return &Response{
		Text:             parsed.Choices[0].Message.Content,
		ModelID:          served,
		RequestID:        httpResp.Header.Get("x-request-id"),
		Latency:          latency,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		Role:             role,
	}, false, nil
}
