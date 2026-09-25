// Package gomobile exports the cubcoder agent as a gomobile-bindable API for
// Android and iOS. It wraps the internal agent/provider/tool loop behind a
// clean, forward-only interface so the native UI can drive the agent without
// needing to understand Go structs or channels.
//
// To rebuild the native bindings, use android/build-binding.sh. It links the
// .so with 16 KB ELF segment alignment (-extldflags=-Wl,-z,max-page-size=16384),
// which is required or the app crashes on launch with UnsatisfiedLinkError on
// 16 KB-page devices (Android 15+). Do NOT run a bare `gomobile bind` without
// that flag on NDK r27 or earlier — it defaults to 4 KB and regresses the crash.
package gomobile

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"cubcoder/internal/agent"
	"cubcoder/internal/config"
	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
	_ "golang.org/x/mobile/bind" // required for gomobile
)

// Callback is the listener interface the Android app implements to receive
// streamed output from the agent. Each method is called on a background
// goroutine; the app must handle threading.
type Callback interface {
	OnText(delta string)      // streamed assistant text (chunk by chunk)
	OnToolCall(name, args string)
	OnToolResult(name, result string)
	OnDone(text string)       // final answer (empty when tool calls remain)
	OnError(err string)       // non-recoverable error
	OnStatus(text string)     // status info (context usage, warnings)
}

// Config holds the connection settings for the model provider.
type Config struct {
	BaseURL      string // e.g. https://ma.cubcloud.ai/v1
	APIKey       string
	Model        string
	AnthropicKey string
	MaxTokens    int
	ContextLimit int
	MaxIters     int
	AutoApprove  bool
}

// Agent wraps cubcoder's model-driven tool loop. Create one per conversation.
type Agent struct {
	a      *agent.Agent
	mu     sync.Mutex
	done   bool
	txCnt  int
	cancel context.CancelFunc // cancels the in-flight Run, if any
}

// callback is the globally registered callback for agent streaming output.
// This avoids passing Callback as a method parameter (which gobind can't handle).
var callback Callback

// RegisterCallback registers a global callback for agent streaming output.
// Called once from the Android app before creating agents.
func RegisterCallback(cb Callback) {
	callback = cb
}

// NewAgent creates a new agent with the given config.
// The agent is ready to run immediately; no initialization call needed.
func NewAgent(cfg *Config) (*Agent, error) {
	c := config.Config{
		BaseURL:      firstNonEmpty(cfg.BaseURL, config.DefaultBaseURL),
		APIKey:       cfg.APIKey,
		Model:        firstNonEmpty(cfg.Model, config.DefaultModel),
		MaxTokens:    firstPositive(cfg.MaxTokens, 32768),
		ContextLimit: cfg.ContextLimit,
		MaxIters:     firstPositive(cfg.MaxIters, config.DefaultMaxIters),
		AutoApprove:  cfg.AutoApprove,
		AnthropicKey: cfg.AnthropicKey,
	}

	p, err := buildProvider(c, c.Model)
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}

	// On Android we use non-interactive tools (no PTY, no sudo watch).
	t := tools.New()

	ag := agent.New(p, t)
	if c.MaxIters > 0 {
		ag.MaxIters = c.MaxIters
	}
	ag.Confirm = func(name, args string) bool { return true } // auto-approve

	return &Agent{a: ag}, nil
}

// Run sends a user message and streams the agent's response through the
// globally registered callback (set via RegisterCallback).
// This starts the agent in a goroutine and returns immediately.
func (ag *Agent) Run(userMsg string) error {
	ag.mu.Lock()
	if ag.done {
		ag.mu.Unlock()
		return fmt.Errorf("agent session ended; create a new agent")
	}
	ag.mu.Unlock()

	if userMsg == "" {
		return fmt.Errorf("empty user message")
	}

	// Get the global callback (must be nil-safe)
	cb := callback
	if cb == nil {
		return fmt.Errorf("callback not registered; call RegisterCallback first")
	}

	// Run agent in background goroutine using the global callback
	go ag.runWithCallback(userMsg, cb)
	return nil
}

// runWithCallback executes the agent loop, intercepting callbacks.
func (ag *Agent) runWithCallback(userMsg string, cb Callback) {
	// Assign fresh handlers each run. Do NOT wrap the existing ag.a.OnText:
	// these handlers are set only here, so wrapping would chain the previous
	// run's closure onto this one and emit every delta an extra time per run
	// (2nd question echoes twice, 3rd thrice, ...).
	ag.a.OnText = func(delta string) {
		cb.OnText(delta)
	}
	ag.a.OnToolCall = func(name, args string) {
		ag.mu.Lock()
		ag.txCnt++
		ag.mu.Unlock()
		cb.OnToolCall(name, args)
	}
	ag.a.OnToolResult = func(name, result string) {
		cb.OnToolResult(name, result)
	}

	// agent.Run requires a non-nil context (it flows down to
	// http.NewRequestWithContext, which rejects a nil ctx). Use a cancellable
	// context so End()/disconnect can abort an in-flight request.
	ctx, cancel := context.WithCancel(context.Background())
	ag.mu.Lock()
	ag.cancel = cancel
	ag.mu.Unlock()
	defer func() {
		cancel()
		ag.mu.Lock()
		ag.cancel = nil
		ag.mu.Unlock()
	}()

	err := ag.a.Run(ctx, userMsg)

	// Send final status
	cb.OnStatus(ag.ContextStatus())

	if err != nil {
		if strings.Contains(err.Error(), "canceled") {
			return
		}
		cb.OnError(err.Error())
	}
}

// Reset clears the conversation but keeps the same agent/provider.
func (ag *Agent) Reset() {
	ag.mu.Lock()
	ag.done = false
	ag.txCnt = 0
	ag.mu.Unlock()
	ag.a.Reset()
}

// End marks the session as ended (useful if the user wants a fresh start).
func (ag *Agent) End() {
	ag.mu.Lock()
	ag.done = true
	if ag.cancel != nil {
		ag.cancel()
	}
	ag.mu.Unlock()
}

// ContextStatus returns the current context usage as a formatted string:
// "used / total tokens (pct%)" or "used tokens (window unknown)".
func (ag *Agent) ContextStatus() string {
	used, total := ag.a.ContextUsage()
	if total == 0 {
		return commas(used) + " tokens (window unknown)"
	}
	return commas(used) + " / " + commas(total) + " tokens (" +
		fmt.Sprintf("%d%%", used*100/total) + ")"
}

// MessageCount returns the number of messages in the conversation.
func (ag *Agent) MessageCount() int {
	return ag.a.MessageCount()
}

// HasUserTurn reports whether the session has any user messages.
func (ag *Agent) HasUserTurn() bool {
	for _, m := range ag.a.Messages() {
		if m.Role == "user" {
			return true
		}
	}
	return false
}

// provider helpers -----------------------------------------------------------

func buildProvider(cfg config.Config, model string) (provider.Provider, error) {
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		if cfg.AnthropicKey == "" {
			return nil, fmt.Errorf("model %q needs an Anthropic key", model)
		}
		c := provider.NewAnthropic(cfg.AnthropicBaseURL, cfg.AnthropicKey, model)
		if cfg.MaxTokens > 0 {
			c.MaxTokens = cfg.MaxTokens
		}
		if cfg.ContextLimit > 0 {
			c.ContextLimit = cfg.ContextLimit
		}
		return c, nil
	}
	o := provider.NewOpenAI(cfg.BaseURL, cfg.APIKey, model)
	if cfg.MaxTokens > 0 {
		o.MaxTokens = cfg.MaxTokens
	}
	if cfg.ContextLimit > 0 {
		o.ContextLimit = cfg.ContextLimit
	}
	return o, nil
}

// utility helpers -----------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func commas(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
