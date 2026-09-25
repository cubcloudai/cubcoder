package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCapOutputTokens(t *testing.T) {
	const limit = 262144
	cases := []struct {
		name       string
		configured int
		input      int
		wantMax    int
		wantOK     bool
	}{
		{"plenty of room passes configured through", 32768, 1000, 32768, true},
		{"unknown limit passes configured through", 32768, 9_000_000, 32768, true /*limit 0*/},
		{"input squeezes output below the ceiling", 32768, limit - 20000, limit - (limit - 20000) - budgetMargin, true},
		{"nearly full fails", 32768, limit - 1000, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lim := limit
			if strings.Contains(c.name, "unknown") {
				lim = 0
			}
			max, ok := CapOutputTokens(c.configured, lim, c.input)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (max=%d)", ok, c.wantOK, max)
			}
			if c.wantOK && max != c.wantMax {
				t.Fatalf("max = %d, want %d", max, c.wantMax)
			}
			if ok && max < budgetFloor {
				t.Fatalf("ok but max %d below floor %d", max, budgetFloor)
			}
		})
	}
}

func TestCapNeverExceedsConfigured(t *testing.T) {
	// A roomy window must not inflate output above the configured ceiling.
	max, ok := CapOutputTokens(8192, 262144, 0)
	if !ok || max != 8192 {
		t.Fatalf("got (%d, %v), want (8192, true)", max, ok)
	}
}

func TestPostWithRetryRecoversFromDroppedConnection(t *testing.T) {
	// First request: server kills the TCP connection mid-request (what a stale
	// keep-alive socket looks like to the client — POST returns EOF). The retry
	// must replay the request and succeed.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close() // abrupt close → client sees EOF
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"ping":1}` {
			t.Errorf("retried body = %q, want original", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := postWithRetry(context.Background(), srv.Client(), srv.URL, []byte(`{"ping":1}`), http.Header{})
	if err != nil {
		t.Fatalf("postWithRetry: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hit %d times, want 2 (drop + retry)", got)
	}
}

func TestPostWithRetryGivesUpOnPersistent5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := postWithRetry(context.Background(), srv.Client(), srv.URL, []byte(`{}`), http.Header{})
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if got := hits.Load(); got != retryAttempts {
		t.Fatalf("server hit %d times, want %d", got, retryAttempts)
	}
}

func TestPostWithRetryDoesNotRetryClientErrors(t *testing.T) {
	// A 400 (e.g. ContextWindowExceededError) is the caller's problem; replaying
	// it would just triple the latency of a doomed request.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	resp, err := postWithRetry(context.Background(), srv.Client(), srv.URL, []byte(`{}`), http.Header{})
	if err != nil {
		t.Fatalf("postWithRetry: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hit %d times, want 1 (no retry on 4xx)", got)
	}
}

func TestPostWithRetryRespectsCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := postWithRetry(ctx, srv.Client(), srv.URL, []byte(`{}`), http.Header{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestEstimateInputTokensGrowsWithContent(t *testing.T) {
	small := EstimateInputTokens([]Message{{Role: "user", Content: "hi"}}, nil)
	big := EstimateInputTokens([]Message{{Role: "user", Content: strings.Repeat("x", 30000)}}, nil)
	if big <= small {
		t.Fatalf("estimate did not grow with content: small=%d big=%d", small, big)
	}
	// 30000 bytes at ~3 bytes/token should land near 10k tokens, not wildly off.
	if big < 8000 || big > 12000 {
		t.Fatalf("estimate %d outside expected range for 30k bytes", big)
	}
}

func TestOpenAIPing(t *testing.T) {
	models := func(ids ...string) string {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%q}`, id)
		}
		b.WriteString("]}")
		return b.String()
	}

	t.Run("model present", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Errorf("path = %q, want /models", r.URL.Path)
			}
			io.WriteString(w, models("other", "Qwen3.6"))
		}))
		defer srv.Close()
		o := NewOpenAI(srv.URL, "", "Qwen3.6")
		if err := o.Ping(context.Background()); err != nil {
			t.Fatalf("Ping = %v, want nil", err)
		}
	})

	t.Run("model missing lists available", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, models("a", "b"))
		}))
		defer srv.Close()
		o := NewOpenAI(srv.URL, "", "missing")
		err := o.Ping(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not registered") {
			t.Fatalf("Ping = %v, want not-registered error", err)
		}
		if !strings.Contains(err.Error(), "a, b") {
			t.Errorf("error should list available models: %v", err)
		}
	})

	t.Run("non-200 surfaces status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()
		o := NewOpenAI(srv.URL, "", "x")
		if err := o.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("Ping = %v, want 500 error", err)
		}
	})

	t.Run("unreachable gateway", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening now
		o := NewOpenAI(url, "", "x")
		if err := o.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("Ping = %v, want unreachable error", err)
		}
	})

	t.Run("reachable but odd shape passes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"object":"list"}`) // no data array
		}))
		defer srv.Close()
		o := NewOpenAI(srv.URL, "", "x")
		if err := o.Ping(context.Background()); err != nil {
			t.Fatalf("Ping = %v, want nil (lenient)", err)
		}
	})
}

// The OpenAI client must ask for stream usage and surface the final usage
// chunk (empty choices) as exact token counts on the Response.
func TestOpenAIChatParsesStreamUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"include_usage":true`) {
			t.Errorf("request must ask for stream usage, got body: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1234,\"completion_tokens\":56}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "key", "model")
	resp, err := o.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q, want %q", resp.Content, "hi")
	}
	if resp.Usage.InputTokens != 1234 || resp.Usage.OutputTokens != 56 {
		t.Errorf("usage = %+v, want input 1234 / output 56", resp.Usage)
	}
}

// The Anthropic stream reports input usage on message_start — split across the
// plain and cache fields, which must be summed to the true prompt size — and
// output usage on message_delta.
func TestAnthropicParseStreamUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":40,"cache_read_input_tokens":860}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"

	a := &Anthropic{}
	resp, err := a.parseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("content = %q, want %q", resp.Content, "hello")
	}
	if resp.Usage.InputTokens != 1000 {
		t.Errorf("input tokens = %d, want 1000 (plain + cache creation + cache read)", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 42 {
		t.Errorf("output tokens = %d, want 42", resp.Usage.OutputTokens)
	}
}

// Images on a tool message can't ride the OpenAI tool role (string content
// only); they must follow that turn's tool results as one user message, before
// the next assistant turn.
func TestOpenAIImagesFollowToolResults(t *testing.T) {
	var got oaiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	o := &OpenAI{BaseURL: srv.URL, Model: "m", HTTP: srv.Client(), MaxTokens: 100}
	img := Image{MediaType: "image/png", Data: "QUJD"}
	_, err := o.Chat(context.Background(), []Message{
		{Role: "user", Content: "look", Images: []Image{img}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "read_file", Args: "{}"}, {ID: "2", Name: "read_file", Args: "{}"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "[image a]", Images: []Image{img}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: "[image b]", Images: []Image{img}},
		{Role: "assistant", Content: "seen"},
		{Role: "user", Content: "next"},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "tool", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	// Tool content stays a string; the synthetic user message carries 2 images.
	if _, ok := got.Messages[2].Content.(string); !ok {
		t.Errorf("tool content should be a string, got %T", got.Messages[2].Content)
	}
	parts, ok := got.Messages[4].Content.([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("image user message content = %#v", got.Messages[4].Content)
	}
	if p := parts[1].(map[string]any); p["type"] != "image_url" || !strings.HasPrefix(p["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,QUJD") {
		t.Errorf("bad image part: %#v", parts[1])
	}
	// The first user message is multimodal, the last is a plain string.
	if _, ok := got.Messages[0].Content.([]any); !ok {
		t.Errorf("user-with-image content should be parts, got %T", got.Messages[0].Content)
	}
	if got.Messages[6].Content != "next" {
		t.Errorf("plain user content = %#v", got.Messages[6].Content)
	}
}

// The Anthropic format carries an image inside the tool_result block itself.
func TestAnthropicImageInToolResult(t *testing.T) {
	var got antRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	a := &Anthropic{BaseURL: srv.URL, Model: "m", HTTP: srv.Client(), MaxTokens: 100}
	img := Image{MediaType: "image/png", Data: "QUJD"}
	_, err := a.Chat(context.Background(), []Message{
		{Role: "user", Content: "look", Images: []Image{img}},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "read_file", Args: "{}"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "[image]", Images: []Image{img}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	if b := got.Messages[0].Content; len(b) != 2 || b[1].Type != "image" || b[1].Source.Data != "QUJD" {
		t.Errorf("user blocks = %+v", b)
	}
	tr := got.Messages[2].Content[0]
	inner, ok := tr.Content.([]any)
	if tr.Type != "tool_result" || !ok || len(inner) != 2 {
		t.Fatalf("tool_result content = %#v", tr.Content)
	}
	if inner[1].(map[string]any)["type"] != "image" {
		t.Errorf("second inner block = %#v", inner[1])
	}
}
