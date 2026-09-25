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

// OpenAI is an OpenAI-compatible chat client. Points at the LiteLLM gateway
// (which speaks the OpenAI wire format) for the local model.
type OpenAI struct {
	BaseURL      string // e.g. https://ma.cubcloud.ai/v1
	APIKey       string
	Model        string
	MaxTokens    int // output cap ceiling; 0 lets the server decide
	ContextLimit int // total context window; caps output to fit. 0 = unknown
	HTTP         *http.Client
}

func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		Model:        model,
		MaxTokens:    8192,   // output ceiling; leaves room for input/history and CapOutputTokens shrinks it further as the convo fills
		ContextLimit: 262144, // total window the LiteLLM gateway serves (vLLM --max-model-len); override with -context-limit if the gateway is reconfigured
		HTTP:         &http.Client{Timeout: 300 * time.Second},
	}
}

func (o *OpenAI) Name() string { return "litellm:" + o.Model }

func (o *OpenAI) ContextWindow() int { return o.ContextLimit }

// Ping checks the gateway is reachable and the configured model is registered,
// via GET /models. It is lenient about the response shape (a reachable gateway
// that answers in an unexpected format is not treated as a failure) — the goal
// is to catch the two common, confusing cases: the gateway is down, or the
// model name is wrong. Honors ctx for a short preflight timeout.
func (o *OpenAI) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s is unreachable (%v); check the gateway is up and -url is correct", o.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway %s returned %d for /models: %s", o.BaseURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Data) == 0 {
		return nil // reachable but unexpected shape / empty list — let the real call speak
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == o.Model {
			return nil
		}
		ids = append(ids, m.ID)
	}
	return fmt.Errorf("model %q is not registered on the gateway; available: %s", o.Model, strings.Join(ids, ", "))
}

// ---- wire types (OpenAI chat completions) ----

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role string `json:"role"`
	// Content is a plain string, or — for a message carrying images — a slice
	// of oaiPart (the multimodal content form).
	Content    any           `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// oaiPart is one element of multimodal message content.
type oaiPart struct {
	Type     string       `json:"type"` // "text" | "image_url"
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"` // data:<media_type>;base64,<data>
}

// oaiContent renders text plus images as multimodal parts; text alone stays a
// plain string, which every OpenAI-compatible server accepts.
func oaiContent(text string, images []Image) any {
	if len(images) == 0 {
		return text
	}
	parts := []oaiPart{}
	if text != "" {
		parts = append(parts, oaiPart{Type: "text", Text: text})
	}
	for _, im := range images {
		parts = append(parts, oaiPart{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:" + im.MediaType + ";base64," + im.Data}})
	}
	return parts
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model      string       `json:"model"`
	Messages   []oaiMessage `json:"messages"`
	Tools      []oaiTool    `json:"tools,omitempty"`
	ToolChoice string       `json:"tool_choice,omitempty"`
	MaxTokens  int          `json:"max_tokens,omitempty"`
	Stream     bool         `json:"stream"`
	// StreamOptions asks for a final chunk carrying exact token usage
	// (supported by vLLM, LiteLLM, and OpenAI; ignored-or-absent elsewhere
	// just means Usage stays zero and callers keep estimating).
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streaming delta chunk
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage arrives on a final chunk with empty choices when
	// stream_options.include_usage was requested.
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAI) Chat(ctx context.Context, messages []Message, tools []ToolSchema, onText StreamFunc) (*Response, error) {
	req := oaiRequest{Model: o.Model, Stream: true, StreamOptions: &oaiStreamOptions{IncludeUsage: true}}
	// Tool messages must carry string content in the OpenAI format, so images
	// produced by a tool (read_file on a PNG) are held back and delivered in one
	// user message right after that turn's block of tool results — a valid
	// assistant → tool… → user sequence that the model sees as "here are the
	// images from those results".
	var pending []Image
	flush := func() {
		if len(pending) == 0 {
			return
		}
		note := "Image from the tool result above:"
		if len(pending) > 1 {
			note = fmt.Sprintf("The %d images from the tool results above, in order:", len(pending))
		}
		req.Messages = append(req.Messages, oaiMessage{Role: "user", Content: oaiContent(note, pending)})
		pending = nil
	}
	for _, m := range messages {
		if m.Role != "tool" {
			flush()
		}
		om := oaiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		switch m.Role {
		case "user":
			om.Content = oaiContent(m.Content, m.Images)
		case "tool":
			pending = append(pending, m.Images...)
		}
		for _, tc := range m.ToolCalls {
			var c oaiToolCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = tc.Args
			om.ToolCalls = append(om.ToolCalls, c)
		}
		req.Messages = append(req.Messages, om)
	}
	flush()
	for _, t := range tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		req.Tools = append(req.Tools, ot)
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}

	inputEst := EstimateInputTokens(messages, tools)
	maxTok, ok := CapOutputTokens(o.MaxTokens, o.ContextLimit, inputEst)
	if !ok {
		return nil, fmt.Errorf("context window nearly full (~%d of %d input tokens used); clear the conversation with /reset or narrow the task", inputEst, o.ContextLimit)
	}
	req.MaxTokens = maxTok

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		header.Set("Authorization", "Bearer "+o.APIKey)
	}
	header.Set("Accept", "text/event-stream")

	resp, err := postWithRetry(ctx, o.HTTP, o.BaseURL+"/chat/completions", body, header)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var content strings.Builder
	type acc struct {
		id, name string
		args     strings.Builder
	}
	var calls []*acc
	var finishReason string
	var usage Usage

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alives / partial lines
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("model error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finishReason = fr
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			content.WriteString(d.Content)
			if onText != nil {
				onText(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			for len(calls) <= tc.Index {
				calls = append(calls, &acc{})
			}
			a := calls[tc.Index]
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream read: %w", err)
	}

	r := &Response{Content: content.String(), Truncated: finishReason == "length", Usage: usage}
	for _, a := range calls {
		if a.name == "" {
			continue
		}
		r.ToolCalls = append(r.ToolCalls, ToolCall{ID: a.id, Name: a.name, Args: a.args.String()})
	}
	return r, nil
}
