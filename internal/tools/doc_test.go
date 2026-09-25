package tools

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExtractDocumentText(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/notes.md"
	if err := os.WriteFile(path, []byte("# Plan\n\nship the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := ExtractDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if text != "# Plan\n\nship the thing\n" {
		t.Errorf("text file must pass through unchanged, got %q", text)
	}
}

func TestExtractDocumentPDF(t *testing.T) {
	text, err := ExtractDocument("testdata/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Revenue") || !strings.Contains(text, "--- page 2 ---") {
		t.Errorf("PDF must extract with page markers:\n%s", text)
	}
}

func TestExtractDocumentBinary(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/blob.bin"
	if err := os.WriteFile(path, []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractDocument(path); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary file must be refused with a 'binary' error, got %v", err)
	}
}

func TestExtractDocumentZipDoc(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/report.docx"
	if err := os.WriteFile(path, []byte("PK\x03\x04rest-of-zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractDocument(path); err == nil || !strings.Contains(err.Error(), "docx") {
		t.Errorf("zip-based document must be refused with format guidance, got %v", err)
	}
}

func TestExtractDocumentDirectory(t *testing.T) {
	if _, err := ExtractDocument(t.TempDir()); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("directory must be refused, got %v", err)
	}
}

func TestSplitDocumentSingleChunk(t *testing.T) {
	chunks := SplitDocument("alpha\nbeta\ngamma\n", 1000)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.StartLine != 1 || c.EndLine != 3 {
		t.Errorf("line range %d-%d, want 1-3", c.StartLine, c.EndLine)
	}
	if c.StartPage != 0 || c.EndPage != 0 {
		t.Errorf("no page markers: page range must be 0-0, got %d-%d", c.StartPage, c.EndPage)
	}
	if c.Text != "alpha\nbeta\ngamma\n" {
		t.Errorf("text mangled: %q", c.Text)
	}
}

// Chunks must cut on line boundaries, cover every line exactly once, and
// reassemble to the original text.
func TestSplitDocumentChunksAreContiguousAndLossless(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line %03d content padding padding padding\n", i)
	}
	text := b.String()
	chunks := SplitDocument(text, 500)
	if len(chunks) < 5 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	var joined strings.Builder
	next := 1
	for _, c := range chunks {
		if c.StartLine != next {
			t.Errorf("chunk starts at line %d, want %d (must be contiguous)", c.StartLine, next)
		}
		if len(c.Text) > 500 {
			t.Errorf("chunk of %d chars exceeds the %d cap", len(c.Text), 500)
		}
		next = c.EndLine + 1
		joined.WriteString(c.Text)
	}
	if joined.String() != text {
		t.Error("chunks must reassemble to the original text")
	}
}

// Page markers set the page range; a chunk boundary falling just before a
// marker attributes the marker's page to the next chunk, not the previous.
func TestSplitDocumentTracksPages(t *testing.T) {
	pad := strings.Repeat("x", 40)
	text := "--- page 1 ---\n" + pad + "\n" + pad + "\n--- page 2 ---\n" + pad + "\n" + pad + "\n"
	// Cap sized so the split lands right before the page 2 marker.
	chunks := SplitDocument(text, 100)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].StartPage != 1 || chunks[0].EndPage != 1 {
		t.Errorf("chunk 1 pages %d-%d, want 1-1", chunks[0].StartPage, chunks[0].EndPage)
	}
	if chunks[1].StartPage != 2 || chunks[1].EndPage != 2 {
		t.Errorf("chunk 2 pages %d-%d, want 2-2", chunks[1].StartPage, chunks[1].EndPage)
	}
}

// A single line longer than the cap becomes its own oversized chunk instead of
// being cut mid-line.
func TestSplitDocumentKeepsLongLinesWhole(t *testing.T) {
	long := strings.Repeat("y", 900)
	chunks := SplitDocument("short\n"+long+"\nshort again\n", 100)
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Text, long) {
			found = true
		}
	}
	if !found {
		t.Error("an over-cap line must survive whole in some chunk")
	}
}
