package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic is a native client for the Claude Messages API. It maps the
// provider-agnostic message/tool model onto Anthropic's wire format, which
// differs from OpenAI's: a top-level system, tool results carried as
// tool_result blocks inside a user turn, and a distinct streaming schema.
// Prompt caching is applied to the stable system+tools prefix.
type Anthropic struct {
	BaseURL      string
	APIKey       string
	Model        string
	MaxTokens    int
	ContextLimit int // total context window; caps output to fit. 0 = unknown
	HTTP         *http.Client
}

func NewAnthropic(baseURL, apiKey, model string) *Anthropic {
	return &Anthropic{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		Model:        model,
		MaxTokens:    32768,
		ContextLimit: 200000, // Claude default window
		HTTP:         &http.Client{Timeout: 300 * time.Second},
	}
}

func (a *Anthropic) Name() string { return "claude:" + a.Model }

func (a *Anthropic) ContextWindow() int { return a.ContextLimit }

// ---- wire types ----

type antCache struct {
	Type string `json:"type"` // "ephemeral"
}

type antBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`          // tool_use
	Name         string          `json:"name,omitempty"`        // tool_use
	Input        json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID    string          `json:"tool_use_id,omitempty"` // tool_result
	Content      any             `json:"content,omitempty"`     // tool_result: string, or []antBlock when it carries images
	Source       *antSource      `json:"source,omitempty"`      // image
	CacheControl *antCache       `json:"cache_control,omitempty"`
}

type antSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// antImageBlocks renders images as image content blocks.
func antImageBlocks(images []Image) []antBlock {
	var out []antBlock
	for _, im := range images {
		out = append(out, antBlock{Type: "image", Source: &antSource{Type: "base64", MediaType: im.MediaType, Data: im.Data}})
	}
	return out
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *antCache      `json:"cache_control,omitempty"`
}

type antRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    []antBlock   `json:"system,omitempty"`
	Messages  []antMessage `json:"messages"`
	Tools     []antTool    `json:"tools,omitempty"`
	Stream    bool         `json:"stream"`
}

func (a *Anthropic) Chat(ctx context.Context, messages []Message, tools []ToolSchema, onText StreamFunc) (*Response, error) {
	inputEst := EstimateInputTokens(messages, tools)
	maxTok, ok := CapOutputTokens(a.MaxTokens, a.ContextLimit, inputEst)
	if !ok {
		return nil, fmt.Errorf("context window nearly full (~%d of %d input tokens used); clear the conversation with /reset or narrow the task", inputEst, a.ContextLimit)
	}
	req := antRequest{Model: a.Model, MaxTokens: maxTok, Stream: true}

	// System: concatenate any system messages; cache the prefix.
	var sysParts []string
	for _, m := range messages {
		if m.Role == "system" && m.Content != "" {
			sysParts = append(sysParts, m.Content)
		}
	}
	if len(sysParts) > 0 {
		req.System = []antBlock{{Type: "text", Text: strings.Join(sysParts, "\n\n"), CacheControl: &antCache{Type: "ephemeral"}}}
	}

	// Tools: cache_control on the last tool caches the whole tools prefix.
	for i, t := range tools {
		at := antTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters}
		if i == len(tools)-1 {
			at.CacheControl = &antCache{Type: "ephemeral"}
		}
		req.Tools = append(req.Tools, at)
	}

	// Messages: translate, coalescing consecutive tool results into one user turn.
	lastToolResult := false
	for _, m := range messages {
		switch m.Role {
		case "system":
			continue
		case "user":
			blocks := []antBlock{}
			if m.Content != "" || len(m.Images) == 0 {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			blocks = append(blocks, antImageBlocks(m.Images)...)
			req.Messages = append(req.Messages, antMessage{Role: "user", Content: blocks})
			lastToolResult = false
		case "assistant":
			var blocks []antBlock
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Args
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				blocks = append(blocks, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage(args)})
			}
			if len(blocks) == 0 {
				// Anthropic rejects empty text blocks; use a space placeholder.
				blocks = append(blocks, antBlock{Type: "text", Text: " "})
			}
			req.Messages = append(req.Messages, antMessage{Role: "assistant", Content: blocks})
			lastToolResult = false
		case "tool":
			trb := antBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if len(m.Images) > 0 {
				// tool_result content may itself be a block list, so the image
				// travels inside the result it belongs to.
				inner := []antBlock{}
				if m.Content != "" {
					inner = append(inner, antBlock{Type: "text", Text: m.Content})
				}
				trb.Content = append(inner, antImageBlocks(m.Images)...)
			}
			if lastToolResult && len(req.Messages) > 0 {
				last := &req.Messages[len(req.Messages)-1]
				last.Content = append(last.Content, trb)
			} else {
				req.Messages = append(req.Messages, antMessage{Role: "user", Content: []antBlock{trb}})
				lastToolResult = true
			}
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("x-api-key", a.APIKey)
	header.Set("anthropic-version", "2023-06-01")

	resp, err := postWithRetry(ctx, a.HTTP, a.BaseURL+"/v1/messages", body, header)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("claude returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	return a.parseStream(resp.Body, onText)
}

// streaming event payloads we care about
type antStreamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"` // carried on message_delta events
	} `json:"delta"`
	// message_start carries input usage; message_delta carries output usage.
	Message *struct {
		Usage antUsage `json:"usage"`
	} `json:"message"`
	Usage *antUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// antUsage is Anthropic's usage block. The true prompt size is the sum of the
// three input fields: input_tokens excludes whatever was served from or
// written to the prompt cache.
type antUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (a *Anthropic) parseStream(r io.Reader, onText StreamFunc) (*Response, error) {
	type acc struct {
		kind string // "text" | "tool_use"
		id   string
		name string
		buf  strings.Builder
	}
	blocks := map[int]*acc{}
	var stopReason string
	var usage Usage

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev antStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				u := ev.Message.Usage
				usage.InputTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
			}
		case "content_block_start":
			b := &acc{kind: "text"}
			if ev.ContentBlock != nil {
				b.kind = ev.ContentBlock.Type
				b.id = ev.ContentBlock.ID
				b.name = ev.ContentBlock.Name
			}
			blocks[ev.Index] = b
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.buf.WriteString(ev.Delta.Text)
				if onText != nil {
					onText(ev.Delta.Text)
				}
			case "input_json_delta":
				b.buf.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			if ev.Error != nil {
				return nil, fmt.Errorf("claude error: %s", ev.Error.Message)
			}
		case "message_stop":
			// done
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream read: %w", err)
	}

	// Assemble in block-index order.
	out := &Response{Truncated: stopReason == "max_tokens", Usage: usage}
	for i := 0; i < len(blocks); i++ {
		b := blocks[i]
		if b == nil {
			continue
		}
		switch b.kind {
		case "text":
			out.Content += b.buf.String()
		case "tool_use":
			args := b.buf.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.id, Name: b.name, Args: args})
		}
	}
	return out, nil
}
