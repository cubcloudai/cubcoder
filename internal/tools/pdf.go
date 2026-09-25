package tools

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// isPDF reports whether the bytes start with the PDF magic header. Detection
// is by content, not filename, so a report saved without a .pdf extension
// still extracts instead of dumping compressed binary at the model.
func isPDF(data []byte) bool {
	return len(data) >= 5 && string(data[:5]) == "%PDF-"
}

// extractPDFText converts a PDF to plain text with page markers. It prefers
// poppler's pdftotext when installed — its -layout mode keeps table columns
// aligned, which matters for the financial reports and invoices these files
// usually are — and falls back to a pure-Go extractor compiled into the
// binary, so a client VM without poppler still reads PDFs.
func extractPDFText(path string) (string, error) {
	if p, err := exec.LookPath("pdftotext"); err == nil {
		// "-" writes to stdout; -q suppresses noise on slightly-damaged files.
		if out, err := exec.Command(p, "-layout", "-q", path, "-").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return pageMarkers(string(out)), nil
		}
		// A poppler failure falls through to the built-in extractor rather
		// than failing the read.
	}
	text, err := extractPDFTextGo(path)
	if err != nil {
		return "", err
	}
	return pageMarkers(text), nil
}

// pageMarkers rewrites form-feed page breaks (emitted by both extractors) as
// visible "--- page N ---" lines, so the model can cite and seek by page.
func pageMarkers(text string) string {
	pages := strings.Split(strings.TrimRight(text, "\f\n"), "\f")
	if len(pages) == 1 {
		return pages[0]
	}
	var b strings.Builder
	for i, p := range pages {
		fmt.Fprintf(&b, "--- page %d ---\n", i+1)
		b.WriteString(strings.Trim(p, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// extractPDFTextGo is the dependency-free path: it reads glyph runs with their
// page coordinates and reconstructs lines (and rough column alignment) from
// them. The library can panic on malformed files, so the panic is converted to
// an error the tool loop can report.
func extractPDFTextGo(path string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse failed: %v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b strings.Builder
	for n := 1; n <= r.NumPage(); n++ {
		p := r.Page(n)
		if p.V.IsNull() {
			continue
		}
		if n > 1 {
			b.WriteString("\f")
		}
		writePageText(&b, p.Content().Text)
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", fmt.Errorf("no extractable text (scanned/image-only PDF?)")
	}
	return b.String(), nil
}

// writePageText lays glyph runs back out as text lines. PDF stores positioned
// fragments, not lines: group fragments sharing a baseline (Y, within a small
// tolerance) into a row, order each row by X, and turn horizontal gaps into
// spaces — proportionally, so table columns in financial statements stay
// visually separated instead of collapsing into one run-on string.
func writePageText(b *strings.Builder, texts []pdf.Text) {
	if len(texts) == 0 {
		return
	}
	// Page origin is bottom-left: larger Y is higher on the page.
	sort.SliceStable(texts, func(i, j int) bool {
		if texts[i].Y != texts[j].Y {
			return texts[i].Y > texts[j].Y
		}
		return texts[i].X < texts[j].X
	})
	const yTol = 2.0
	var row []pdf.Text
	rowY := texts[0].Y
	flush := func() {
		if len(row) == 0 {
			return
		}
		sort.SliceStable(row, func(i, j int) bool { return row[i].X < row[j].X })
		end := row[0].X // right edge of what's been written so far
		for i, t := range row {
			charW := t.FontSize * 0.5
			if charW <= 0 {
				charW = 5
			}
			if gap := t.X - end; i > 0 && gap > charW*0.4 {
				n := int(gap / charW)
				if n < 1 {
					n = 1
				}
				b.WriteString(strings.Repeat(" ", n))
			}
			b.WriteString(t.S)
			if e := t.X + t.W; e > end {
				end = e
			}
		}
		b.WriteString("\n")
	}
	for _, t := range texts {
		if rowY-t.Y > yTol {
			flush()
			row = row[:0]
			rowY = t.Y
		}
		row = append(row, t)
	}
	flush()
}
