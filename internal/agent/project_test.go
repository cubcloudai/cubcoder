package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectInstructionsMissingFile(t *testing.T) {
	name, block := ProjectInstructions(t.TempDir())
	if name != "" || block != "" {
		t.Fatalf("expected empty result for missing file, got name=%q block=%q", name, block)
	}
}

func TestProjectInstructionsLoadsAndFormats(t *testing.T) {
	dir := t.TempDir()
	content := "Run `make test` before committing.\nUse tabs."
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, block := ProjectInstructions(dir)
	if name != "AGENTS.md" {
		t.Fatalf("name = %q, want AGENTS.md", name)
	}
	if !strings.Contains(block, content) {
		t.Fatalf("block missing file content: %q", block)
	}
	if !strings.Contains(block, "--- AGENTS.md ---") || !strings.Contains(block, "--- end of AGENTS.md ---") {
		t.Fatalf("block missing fence markers: %q", block)
	}
}

func TestProjectInstructionsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Agents.MD"), []byte("mixed casing"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, block := ProjectInstructions(dir)
	if name != "Agents.MD" {
		t.Fatalf("name = %q, want Agents.MD", name)
	}
	if !strings.Contains(block, "mixed casing") {
		t.Fatalf("block missing content: %q", block)
	}
}

func TestProjectInstructionsPrefersExactCasing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.md"), []byte("lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, _ := ProjectInstructions(dir)
	if name != "AGENTS.md" {
		t.Fatalf("name = %q, want AGENTS.md preferred over agents.md", name)
	}
}

func TestProjectInstructionsIgnoresEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agents.md"), []byte("  \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, block := ProjectInstructions(dir)
	if name != "" || block != "" {
		t.Fatalf("expected empty result for whitespace-only file, got name=%q block=%q", name, block)
	}
}

func TestProjectInstructionsTruncatesOversized(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", projectInstructionsCap+100)
	if err := os.WriteFile(filepath.Join(dir, "agents.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	_, block := ProjectInstructions(dir)
	if !strings.Contains(block, "[truncated]") {
		t.Fatal("oversized file should be marked truncated")
	}
	if len(block) > projectInstructionsCap+1024 {
		t.Fatalf("block not truncated: %d bytes", len(block))
	}
}
