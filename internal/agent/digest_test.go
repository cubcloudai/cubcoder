package agent

import (
	"context"
	"strings"
	"testing"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

// A document split into two chunks must produce a digest with both chunk
// summaries, labeled line ranges, the file path, and the read_file guidance —
// everything the model needs to navigate back into the original.
func TestDigestDocumentBuildsNavigableDigest(t *testing.T) {
	stub := &stubProvider{window: 200000, responses: []*provider.Response{
		{Content: "SUMMARY-ONE"},
		{Content: "SUMMARY-TWO"},
	}}
	a := New(stub, tools.New())

	line := strings.Repeat("z", 100) + "\n"
	text := strings.Repeat(line, 600) // ~60k chars → two chunks at the ~49k default

	digest, err := a.DigestDocument(context.Background(), "big.txt", "/data/big.txt", text)
	if err != nil {
		t.Fatalf("DigestDocument: %v", err)
	}
	for _, want := range []string{"SUMMARY-ONE", "SUMMARY-TWO", "/data/big.txt", "read_file"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q", want)
		}
	}
	if got := strings.Count(digest, "## lines "); got != 2 {
		t.Errorf("digest has %d labeled sections, want 2:\n%s", got, digest)
	}
	if stub.calls != 2 {
		t.Errorf("model called %d times, want once per chunk (2)", stub.calls)
	}
	// The last chunk's label must reach the document's final line.
	if !strings.Contains(digest, "–600") {
		t.Errorf("last section label should end at line 600:\n%s", digest)
	}
}

// Page markers in the text must surface as page ranges in the section labels.
func TestDigestDocumentLabelsPages(t *testing.T) {
	stub := &stubProvider{window: 200000, responses: []*provider.Response{{Content: "S"}}}
	a := New(stub, tools.New())

	text := "--- page 1 ---\nintro\n--- page 2 ---\nbody\n--- page 3 ---\nend\n"
	digest, err := a.DigestDocument(context.Background(), "doc.pdf", "/data/doc.pdf", text)
	if err != nil {
		t.Fatalf("DigestDocument: %v", err)
	}
	if !strings.Contains(digest, "(pages 1–3)") {
		t.Errorf("section label must carry the page range:\n%s", digest)
	}
}

// An empty summary from the model must fail the digest rather than attach a
// blind spot: a section the digest silently says nothing about.
func TestDigestDocumentFailsOnEmptySummary(t *testing.T) {
	stub := &stubProvider{window: 200000, responses: []*provider.Response{{Content: "   "}}}
	a := New(stub, tools.New())

	_, err := a.DigestDocument(context.Background(), "doc.txt", "/data/doc.txt", "some text\n")
	if err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("want an empty-summary error, got %v", err)
	}
}

// A document that would take more than the chunk budget even at the window's
// maximum chunk size is refused with a clear error, not silently truncated.
func TestDigestDocumentRefusesAbsurdSize(t *testing.T) {
	a := New(&stubProvider{window: 1000}, tools.New())         // tiny window → tiny max chunk
	text := strings.Repeat(strings.Repeat("q", 99)+"\n", 1500) // 150k chars ≫ 64 chunks × ~2.2k

	_, err := a.DigestDocument(context.Background(), "huge.txt", "/data/huge.txt", text)
	if err == nil || !strings.Contains(err.Error(), "too large to digest") {
		t.Fatalf("want a too-large-to-digest error, got %v", err)
	}
}
