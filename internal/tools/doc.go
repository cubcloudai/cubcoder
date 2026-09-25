package tools

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// maxDocumentBytes caps how large a file /attach will read. Far past any
// context window; this only guards against pointing /attach at a giant
// artifact (a database dump, a video) by mistake.
const maxDocumentBytes = 20 << 20

// ExtractDocument reads a user-supplied document for prompt attachment and
// returns its plain text. PDFs are extracted through the same pipeline as
// read_file (page markers, table layout preserved); text files pass through
// unchanged. Binary formats return an error with guidance instead of feeding
// the model garbage.
func ExtractDocument(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	if fi.Size() > maxDocumentBytes {
		return "", fmt.Errorf("%s is %d MiB — too large to attach (max %d MiB)", path, fi.Size()>>20, maxDocumentBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if isPDF(data) {
		return extractPDFText(path)
	}
	if isImage(data) {
		return "", fmt.Errorf("%s is an image — attach it with /attach and it rides your next message as a picture, or ask the model to read_file it", path)
	}
	// Office documents (docx/xlsx/pptx/odt) are zip archives; naming them beats
	// a generic "binary" error.
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return "", fmt.Errorf("%s looks like a zip-based document (docx/xlsx/odt?) — export it to PDF or plain text first", path)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return "", fmt.Errorf("%s is binary — only text files and PDFs can be attached", path)
	}
	return string(data), nil
}

// DocChunk is one contiguous slice of a document, with the line range (and
// page range, when the document has "--- page N ---" markers) that locates it
// in the original — the coordinates a read_file follow-up needs.
type DocChunk struct {
	Text      string
	StartLine int // 1-indexed, inclusive
	EndLine   int
	StartPage int // 0 when the document has no page markers
	EndPage   int
}

// SplitDocument cuts text into chunks of at most chunkChars each, on line
// boundaries, tracking the line and page ranges of every chunk. A single line
// longer than chunkChars becomes its own oversized chunk rather than being
// split mid-line.
func SplitDocument(text string, chunkChars int) []DocChunk {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var chunks []DocChunk
	page := 0
	var b strings.Builder
	cur := DocChunk{StartLine: 1}
	flush := func(endLine int) {
		if b.Len() == 0 {
			return
		}
		cur.Text = b.String()
		cur.EndLine = endLine
		cur.EndPage = page
		chunks = append(chunks, cur)
		b.Reset()
	}
	for i, line := range lines {
		// Flush before parsing this line's page marker: a chunk ending just
		// before "--- page N ---" belongs to the previous page.
		if b.Len() > 0 && b.Len()+len(line)+1 > chunkChars {
			flush(i) // previous line, 1-indexed
			cur = DocChunk{StartLine: i + 1}
		}
		if n := pageMarkerNumber(line); n > 0 {
			page = n
		}
		if b.Len() == 0 {
			cur.StartPage = page
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush(len(lines))
	return chunks
}

// pageMarkerNumber parses a "--- page N ---" line emitted by the PDF
// extractor; 0 means the line is not a page marker.
func pageMarkerNumber(line string) int {
	var n int
	if _, err := fmt.Sscanf(line, "--- page %d ---", &n); err == nil && strings.HasPrefix(line, "--- page ") {
		return n
	}
	return 0
}
