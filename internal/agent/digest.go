package agent

// Big-document ingestion. A document too large to attach verbatim can never
// ride a message (a single oversized message is past what compaction can ever
// reclaim), so it is digested instead: split into chunks, each chunk
// summarized by the model, and the summaries assembled into a compact digest
// that carries line and page pointers back into the original file. The model
// reads any section verbatim with read_file when detail matters.

import (
	"context"
	"fmt"
	"strings"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

const (
	// digestChunkTokens is the target chunk size. Big enough that a chunk
	// carries whole sections of a document, small enough that dozens of them
	// stay affordable.
	digestChunkTokens = 16384
	// digestMaxChunks bounds the number of model calls one /attach can spend.
	// Chunks grow past the target size (window permitting) to keep very large
	// documents under this count.
	digestMaxChunks = 64
	// digestSummaryCapChars bounds one chunk's summary, so a model that
	// ignores the length instruction cannot bloat the digest past attaching.
	digestSummaryCapChars = 4000
	// digestCharsPerToken mirrors the provider package's conservative
	// byte-per-token ratio for sizing digest chunks.
	digestCharsPerToken = 3
)

const digestSystemPrompt = `You are indexing one section of a large document so an assistant can navigate the document later without reading all of it. Write a dense summary of the section: its topics, key facts, figures, names, dates, and section headings, exactly as stated. Do not interpret or editorialize. Keep it under 400 words. Output only the summary, with no preamble.`

// DigestDocument summarizes text chunk by chunk into a digest that fits the
// attach budget, labeling every chunk summary with the line (and page) range
// it covers in the file at path. name is the display name used in the fences.
func (a *Agent) DigestDocument(ctx context.Context, name, path, text string) (string, error) {
	window := a.Provider.ContextWindow()

	// Size the chunks: start at the target, grow to keep the count under
	// digestMaxChunks, but never past what a summarize request can carry.
	chunkChars := digestChunkTokens * digestCharsPerToken
	if need := (len(text) + digestMaxChunks - 1) / digestMaxChunks; need > chunkChars {
		chunkChars = need
	}
	if window > 0 {
		// Leave room for the digest system prompt, template overhead, the
		// provider's budget margin, and the summary output; scale the reserve
		// down for small windows.
		reserve := window / 2
		if reserve > 24576 {
			reserve = 24576
		}
		maxChunkChars := (window - reserve) * digestCharsPerToken
		if maxChunkChars <= 0 {
			return "", fmt.Errorf("the %d-token context window is too small to digest a document", window)
		}
		if chunkChars > maxChunkChars {
			if (len(text)+maxChunkChars-1)/maxChunkChars > digestMaxChunks {
				return "", fmt.Errorf("%s is too large to digest: it would take more than %d chunks even at the window's maximum chunk size", name, digestMaxChunks)
			}
			chunkChars = maxChunkChars
		}
	}

	chunks := tools.SplitDocument(text, chunkChars)
	if len(chunks) == 0 {
		return "", fmt.Errorf("%s is empty", name)
	}

	// No fences here: the attachment machinery fences the digest like any
	// other document when it rides the message.
	var b strings.Builder
	fmt.Fprintf(&b, "This is a digest of %s, which was too large to attach in full: each section below was summarized. The full text is the file %s — call read_file with offset/limit set to a section's line range to read it verbatim when detail matters.\n", name, path)
	for i, c := range chunks {
		a.spin(fmt.Sprintf("digesting %d/%d", i+1, len(chunks)))
		summary, err := a.summarizeChunk(ctx, name, c, i+1, len(chunks))
		a.spinStop()
		if err != nil {
			return "", fmt.Errorf("digest chunk %d/%d: %w", i+1, len(chunks), err)
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", rangeLabel(c), strings.TrimSpace(summary))
	}
	return b.String(), nil
}

// summarizeChunk asks the model to index one chunk. No tools are advertised,
// so the response cannot trip the truncated-tool-call path.
func (a *Agent) summarizeChunk(ctx context.Context, name string, c tools.DocChunk, i, n int) (string, error) {
	prompt := fmt.Sprintf("Section %d of %d of the document %q (%s):\n\n%s", i, n, name, rangeLabel(c), c.Text)
	resp, err := a.Provider.Chat(ctx, []provider.Message{
		{Role: "system", Content: digestSystemPrompt},
		{Role: "user", Content: prompt},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("model returned an empty summary")
	}
	return cutMiddle(resp.Content, digestSummaryCapChars, digestSummaryCapChars/4,
		"\n... [%d bytes of this summary omitted] ...\n"), nil
}

// rangeLabel renders a chunk's location: "lines 1201–2400 (pages 81–160)".
func rangeLabel(c tools.DocChunk) string {
	s := fmt.Sprintf("lines %d–%d", c.StartLine, c.EndLine)
	switch {
	case c.StartPage > 0 && c.EndPage > c.StartPage:
		s += fmt.Sprintf(" (pages %d–%d)", c.StartPage, c.EndPage)
	case c.StartPage > 0:
		s += fmt.Sprintf(" (page %d)", c.StartPage)
	}
	return s
}
