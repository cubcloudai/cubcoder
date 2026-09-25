// Package provider abstracts the model backend behind a single interface so
// the agent loop is provider-agnostic. The MVP ships an OpenAI-compatible
// client (LiteLLM → local model); a native Anthropic client is added later
// behind the same interface.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Message is a provider-agnostic chat message.
// JSON tags are the on-disk session format (~/.cubcoder/sessions); keep them
// stable so old sessions keep loading.
type Message struct {
	Role       string     `json:"role"`                   // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`                // text content (may be empty when ToolCalls is set)
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant-issued tool calls
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on a "tool" message: which call it answers
	Name       string     `json:"name,omitempty"`         // set on a "tool" message: the tool name
	// Images ride alongside the text on "user" and "tool" messages (an attached
	// screenshot; a read_file of a PNG). Each backend maps them to its own
	// image block form.
	Images []Image `json:"images,omitempty"`
}

// Image is one inline image: a supported media type and its base64 payload.
type Image struct {
	MediaType string `json:"media_type"` // image/png, image/jpeg, image/gif, image/webp
	Data      string `json:"data"`       // standard base64, no data: prefix
}

// ImageTokenEstimate is the input cost charged per image by the estimator. It
// matches a full-size (~1.15 MP) image on the Anthropic scale (w*h/750) and
// is in the same range for vision-capable local models.
const ImageTokenEstimate = 1600

// ToolCall is a model request to run a tool.
type ToolCall struct {
	ID   string `json:"id"` // provider call id (echoed back on the tool result)
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments
}

// ToolSchema describes a tool to the model (JSON Schema parameters).
type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Response is one model turn: either text, tool calls, or both.
type Response struct {
	Content   string
	ToolCalls []ToolCall
	// Truncated is set when the model stopped because it hit the output token
	// limit (OpenAI finish_reason "length" / Anthropic stop_reason "max_tokens").
	// A turn cut off mid-tool-call has incomplete JSON arguments, so the agent
	// must not record or execute them.
	Truncated bool
	// Usage is the server's exact token accounting for this request, when the
	// backend reports it. Zero values mean unknown; callers must fall back to
	// EstimateInputTokens.
	Usage Usage
}

// Usage is the server-reported token count for one request. InputTokens is the
// full prompt as the server tokenized it (history + tool schemas + template
// overhead — everything the estimator can only guess at).
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// StreamFunc receives assistant text as it streams. May be nil.
type StreamFunc func(textDelta string)

// Token-budget tuning. The input estimate is deliberately rough: we don't ship
// the model's tokenizer, so we overestimate (code packs more tokens per byte
// than prose) and keep a margin for chat-template overhead the estimate misses.
const (
	estCharsPerToken = 3    // bytes per token; low ⇒ overestimate input ⇒ safer cap
	estPerMessage    = 4    // structural tokens (role markers, wrappers) per message
	budgetMargin     = 4096 // headroom for estimation error and template overhead
	budgetFloor      = 512  // below this, a turn isn't worth sending
)

// EstimateInputTokens approximates the prompt size of a turn so a provider can
// shrink its output cap to fit the model's context window. It counts message
// and tool-schema bytes, converts at a conservative ratio, and adds per-message
// structural overhead. It overestimates on purpose; exact accounting would need
// the model's own tokenizer.
func EstimateInputTokens(messages []Message, tools []ToolSchema) int {
	chars, images := 0, 0
	for _, m := range messages {
		chars += len(m.Role) + len(m.Content) + len(m.Name) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			chars += len(tc.ID) + len(tc.Name) + len(tc.Args)
		}
		images += len(m.Images)
	}
	for _, t := range tools {
		chars += len(t.Name) + len(t.Description)
		if b, err := json.Marshal(t.Parameters); err == nil {
			chars += len(b)
		}
	}
	return chars/estCharsPerToken + len(messages)*estPerMessage + images*ImageTokenEstimate
}

// CapOutputTokens returns how many output tokens to request: the configured
// ceiling, shrunk to whatever the context window has left after the input. A
// contextLimit of 0 means "unknown" and the configured value passes through. ok
// is false when the remaining budget is below budgetFloor — the caller should
// surface a clear "context full" error rather than send a doomed request.
func CapOutputTokens(configured, contextLimit, inputTokens int) (max int, ok bool) {
	if contextLimit <= 0 {
		return configured, true
	}
	max = configured
	if budget := contextLimit - inputTokens - budgetMargin; budget < max {
		max = budget
	}
	if max < budgetFloor {
		return max, false
	}
	return max, true
}

// Retry tuning for postWithRetry. Connection-level failures and gateway 5xxs
// are retried with a short backoff; everything else returns immediately.
const (
	retryAttempts = 3 // total tries, not extra retries
	retryBaseWait = 250 * time.Millisecond
)

// retryableErr reports whether a request failed for a transient transport
// reason worth replaying: most commonly a stale keep-alive socket (the LiteLLM
// gateway's uvicorn closes idle connections after ~5s, so any pooled connection
// goes dead while a tool call runs and the next POST reads EOF). Go's transport
// won't replay POSTs on its own, so we do it here — safe because the failure
// happens before any response bytes reach the caller.
func retryableErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// url.Error wraps transport errors in ways errors.Is can miss (e.g. a bare
	// "EOF" string from http2); fall back to a message check.
	msg := err.Error()
	return strings.Contains(msg, "EOF") || strings.Contains(msg, "connection reset")
}

// postWithRetry POSTs body to url, replaying on transient transport errors and
// gateway 502/503/504s. The body is a byte slice so every attempt rebuilds the
// request from scratch. On success the caller owns resp.Body.
func postWithRetry(ctx context.Context, client *http.Client, url string, body []byte, header http.Header) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBaseWait << (attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := client.Do(req)
		if err != nil {
			if retryableErr(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("after %d attempts: %w", retryAttempts, lastErr)
}

// Provider is a model backend.
type Provider interface {
	// Chat sends the conversation plus available tools and returns one turn.
	// If onText is non-nil, assistant text is delivered incrementally as it
	// streams; the assembled turn (text + tool calls) is still returned.
	Chat(ctx context.Context, messages []Message, tools []ToolSchema, onText StreamFunc) (*Response, error)
	// Name identifies the backend (for logs / status).
	Name() string
	// ContextWindow is the model's total token budget (input + output), used to
	// decide when to compact history. 0 means unknown.
	ContextWindow() int
}

// Pinger is an optional Provider capability: a cheap startup reachability and
// model-availability check. The CLI runs it as a preflight so a down gateway or
// an unregistered model surfaces immediately, with guidance, rather than as a
// terse failure on the first real request. Backends that can't check cheaply
// (or would spend tokens doing so) simply don't implement it.
type Pinger interface {
	// Ping returns nil when the backend is reachable and the configured model is
	// available, or an error describing what to fix.
	Ping(ctx context.Context) error
}
