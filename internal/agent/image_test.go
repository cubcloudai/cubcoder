package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

// A 1x1 PNG, the smallest real image; enough for content sniffing.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

// read_file on an image must put the picture on the tool message, not in the
// text; and a second read of the same path must evict the first image's bytes.
func TestReadFileImageRidesToolMessageAndEvicts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	stub := &stubProvider{responses: []*provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Args: `{"path":"shot.png"}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "2", Name: "read_file", Args: `{"path":"shot.png"}`}}},
		{Content: "done"},
	}}
	a := New(stub, tools.NewIn(dir))
	if err := a.Run(context.Background(), "look at the screenshot"); err != nil {
		t.Fatal(err)
	}
	var toolMsgs []provider.Message
	for _, m := range a.Messages() {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("tool messages = %d", len(toolMsgs))
	}
	for _, m := range toolMsgs {
		if len(m.Images) != 1 || m.Images[0].MediaType != "image/png" {
			t.Errorf("tool message lacks image: %+v", m.Images)
		}
		if !strings.Contains(m.Content, "1x1") || !strings.Contains(m.Content, "image/png") {
			t.Errorf("text should describe the image, got %q", m.Content)
		}
	}
	// Eviction: the older duplicate read loses its image; the newest keeps it.
	if n := a.evictStaleReads(); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	msgs := a.Messages()
	var after []provider.Message
	for _, m := range msgs {
		if m.Role == "tool" {
			after = append(after, m)
		}
	}
	if len(after[0].Images) != 0 || !strings.Contains(after[0].Content, "superseded") {
		t.Errorf("old image read not evicted: %+v", after[0])
	}
	if len(after[1].Images) != 1 {
		t.Error("newest image read must keep its image")
	}
}

// An attached image rides the next user message as image content with a
// fenced description in the text, and is consumed by that turn.
func TestAttachImage(t *testing.T) {
	stub := &stubProvider{responses: []*provider.Response{{Content: "ok"}, {Content: "ok"}}}
	a := New(stub, tools.New())
	a.AttachImage("shot.png", "image/png, 1x1, 70 B", provider.Image{MediaType: "image/png", Data: "AAAA"})
	if used, _ := a.ContextUsage(); used < provider.ImageTokenEstimate {
		t.Errorf("staged image not counted in context usage: %d", used)
	}
	if err := a.Run(context.Background(), "what is this"); err != nil {
		t.Fatal(err)
	}
	u := stub.lastSent[len(stub.lastSent)-1]
	if u.Role != "user" || len(u.Images) != 1 || !strings.Contains(u.Content, "attached image 1: shot.png") || !strings.HasSuffix(u.Content, "what is this") {
		t.Errorf("user message = %+v", u)
	}
	if err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if u := stub.lastSent[len(stub.lastSent)-1]; len(u.Images) != 0 {
		t.Error("image leaked into a later turn")
	}
}

// Restore swaps in a saved transcript under the agent's current system prompt.
func TestRestoreKeepsCurrentSystemPrompt(t *testing.T) {
	a := NewWithSystem(&stubProvider{}, tools.New(), "NEW SYSTEM")
	a.Restore([]provider.Message{
		{Role: "system", Content: "OLD SYSTEM"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})
	m := a.Messages()
	if len(m) != 3 || m[0].Content != "NEW SYSTEM" || m[1].Content != "hi" {
		t.Errorf("restored = %+v", m)
	}
}
