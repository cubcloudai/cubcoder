// Package config resolves cubcoder settings. Precedence: flags > env >
// config file (~/.cubcoder/config.json or $CUBCODER_CONFIG) > defaults.
// MVP targets the local model via the LiteLLM gateway. Claude is added later.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	BaseURL      string // LiteLLM/OpenAI-compatible base, ending in /v1
	APIKey       string // engagement virtual key
	Model        string
	MaxTokens    int  // output token cap per turn
	ContextLimit int  // total context window; 0 = use the backend's own default
	MaxIters     int  // max tool-loop iterations per request
	AutoApprove  bool // skip permission prompts for writes/commands

	// CommandTimeout is the default run_command timeout in seconds; 0 keeps the
	// tools package's built-in default (120s). The model can raise it per call
	// with the tool's timeout argument, up to the tools package's cap.
	CommandTimeout int

	// Claude (Anthropic) backend — used when the model name starts with "claude".
	AnthropicKey     string
	AnthropicBaseURL string

	// SearchURL is the self-hosted SearXNG base the web_search tool queries.
	SearchURL string
}

const (
	DefaultBaseURL          = "https://ma.cubcloud.ai/v1"
	DefaultModel            = "Qwen3.6-35B-A3B-FP8"
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	// DefaultMaxTokens overrides LiteLLM's 4096 fallback output cap, which
	// truncates file writes mid-call. The local Qwen model has a 262k context
	// with output guidance in the 32k–81k range; 32k keeps writes from
	// truncating while leaving the bulk of the window for input.
	DefaultMaxTokens = 32768
	// DefaultMaxIters bounds the tool loop per request. A single request can fan
	// out a lot — an orchestration turn spends iterations on delegate/wait calls
	// and then on hands-on edits — and a local model takes more, smaller steps
	// than a frontier one, so the cap is generous. It exists only as a
	// runaway-loop backstop, not a normal stopping point.
	DefaultMaxIters = 150
)

// FileConfig is the on-disk config (~/.cubcoder/config.json). Exported so the
// `login` and `config set` CLI commands can read, edit, and rewrite it without
// disturbing fields they don't touch.
type FileConfig struct {
	BaseURL          string `json:"base_url,omitempty"`
	APIKey           string `json:"api_key,omitempty"`
	Model            string `json:"model,omitempty"`
	MaxTokens        int    `json:"max_tokens,omitempty"`
	ContextLimit     int    `json:"context_limit,omitempty"`
	MaxIters         int    `json:"max_iters,omitempty"`
	CommandTimeout   int    `json:"command_timeout,omitempty"`
	AnthropicKey     string `json:"anthropic_api_key,omitempty"`
	AnthropicBaseURL string `json:"anthropic_base_url,omitempty"`
	SearchURL        string `json:"search_url,omitempty"`
}

// Path is the config file location: $CUBCODER_CONFIG if set, else
// ~/.cubcoder/config.json.
func Path() (string, error) {
	if p := os.Getenv("CUBCODER_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cubcoder", "config.json"), nil
}

// LoadFile reads the config file for editing. A missing file is not an error:
// it returns a zero FileConfig so callers can populate and Save it. A malformed
// file is an error so `config set` never silently clobbers it.
func LoadFile() (FileConfig, error) {
	path, err := Path()
	if err != nil {
		return FileConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileConfig{}, nil
		}
		return FileConfig{}, err
	}
	var fc FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return FileConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return fc, nil
}

// Save writes fc to the config file as indented JSON, creating ~/.cubcoder
// (0700) and the file (0600) since it holds API keys. Returns the path written.
func (fc FileConfig) Save() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// SetField sets one field by its JSON name (e.g. "api_key", "model",
// "max_tokens"). Int fields parse their value; an unknown field is an error.
func (fc *FileConfig) SetField(name, value string) error {
	switch name {
	case "base_url":
		fc.BaseURL = value
	case "api_key":
		fc.APIKey = value
	case "model":
		fc.Model = value
	case "anthropic_api_key":
		fc.AnthropicKey = value
	case "anthropic_base_url":
		fc.AnthropicBaseURL = value
	case "search_url":
		fc.SearchURL = value
	case "max_tokens", "context_limit", "max_iters", "command_timeout":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer: %w", name, err)
		}
		switch name {
		case "max_tokens":
			fc.MaxTokens = n
		case "context_limit":
			fc.ContextLimit = n
		case "max_iters":
			fc.MaxIters = n
		case "command_timeout":
			fc.CommandTimeout = n
		}
	default:
		return fmt.Errorf("unknown field %q (api_key, model, base_url, anthropic_api_key, anthropic_base_url, search_url, max_tokens, context_limit, max_iters, command_timeout)", name)
	}
	return nil
}

func loadFile() FileConfig {
	fc, _ := LoadFile()
	return fc
}

// Load resolves config: explicit args > CUBCODER_* env > config file > defaults.
func Load(baseURL, apiKey, model, anthropicKey string, maxTokens, contextLimit, maxIters, commandTimeout int, autoApprove bool) Config {
	fc := loadFile()
	return Config{
		BaseURL:          firstNonEmpty(baseURL, os.Getenv("CUBCODER_API_URL"), fc.BaseURL, DefaultBaseURL),
		APIKey:           firstNonEmpty(apiKey, os.Getenv("CUBCODER_API_KEY"), fc.APIKey),
		Model:            firstNonEmpty(model, os.Getenv("CUBCODER_MODEL"), fc.Model, DefaultModel),
		MaxTokens:        firstPositive(maxTokens, atoi(os.Getenv("CUBCODER_MAX_TOKENS")), fc.MaxTokens, DefaultMaxTokens),
		ContextLimit:     firstPositive(contextLimit, atoi(os.Getenv("CUBCODER_CONTEXT_LIMIT")), fc.ContextLimit), // 0 ⇒ backend default
		MaxIters:         firstPositive(maxIters, atoi(os.Getenv("CUBCODER_MAX_ITERS")), fc.MaxIters, DefaultMaxIters),
		CommandTimeout:   firstPositive(commandTimeout, atoi(os.Getenv("CUBCODER_COMMAND_TIMEOUT")), fc.CommandTimeout), // 0 ⇒ tools default
		AutoApprove:      autoApprove,
		AnthropicKey:     firstNonEmpty(anthropicKey, os.Getenv("CUBCODER_ANTHROPIC_KEY"), fc.AnthropicKey),
		AnthropicBaseURL: firstNonEmpty(os.Getenv("CUBCODER_ANTHROPIC_URL"), fc.AnthropicBaseURL, DefaultAnthropicBaseURL),
		// Empty leaves the tools package on its built-in DefaultSearchURL; main
		// injects this via tools.SetSearchURL, which ignores a blank value.
		SearchURL: firstNonEmpty(os.Getenv("CUBCODER_SEARCH_URL"), fc.SearchURL),
	}
}

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

// atoi parses an int, returning 0 on empty/invalid input.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
