package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The fixture is a two-page financial report (testdata/report.pdf) with
// tabular income-statement and balance-sheet data — the shape of document this
// feature exists for.

func TestExtractPDFText(t *testing.T) {
	text, err := extractPDFText("testdata/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	assertReportText(t, text)
}

// The pure-Go fallback must hold up on its own: client VMs may not have
// poppler installed.
func TestExtractPDFTextGo(t *testing.T) {
	text, err := extractPDFTextGo("testdata/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	assertReportText(t, pageMarkers(text))
}

func assertReportText(t *testing.T, text string) {
	t.Helper()
	for _, want := range []string{
		"Acme Holdings",
		"Revenue", "1,842,300",
		"Net Income", "597,650",
		"--- page 2 ---",
		"Shareholder Equity", "2,895,200",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("extracted text missing %q\n--- got ---\n%s", want, text)
		}
	}
	// Numbers must land on the same line as their label or the table is useless.
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Net Income") && !strings.Contains(line, "597,650") {
			t.Errorf("Net Income row lost its value: %q", line)
		}
	}
}

// read_file must route PDFs through extraction (detected by magic bytes, not
// extension) and keep its offset/limit paging behavior.
func TestReadFilePDF(t *testing.T) {
	tool := readFile("")
	args, _ := json.Marshal(map[string]any{"path": "testdata/report.pdf"})
	out, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1,842,300") || !strings.Contains(out, "--- page 1 ---") {
		t.Errorf("read_file did not extract PDF text:\n%s", out)
	}

	// Same file under a non-.pdf name still extracts (content sniffing).
	data, err := os.ReadFile("testdata/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	renamed := dir + "/report.dat"
	if err := os.WriteFile(renamed, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Run(context.Background(), `{"path":`+strconvQuote(renamed)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Revenue") {
		t.Errorf("magic-byte detection failed for non-.pdf name:\n%s", out)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
