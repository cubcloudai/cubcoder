// Project instructions: an agents.md file in the working directory carries
// repo-specific guidance (build commands, conventions, layout). It is loaded
// once at startup and appended to the system prompt, so it survives /reset and
// compaction (both preserve the system message byte-identical).
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectInstructionsCap bounds how much of an agents.md rides in the system
// prompt. Instructions files are normally a page or two; the cap only guards
// against a pathological file eating the served context window.
const projectInstructionsCap = 32 * 1024

// ProjectInstructions looks for an agents.md file in dir (any casing, with
// AGENTS.md preferred when several casings exist) and returns its filename and
// a block ready to append to a system prompt. Both are empty when no readable,
// non-empty file exists — a missing file is the normal case, not an error.
func ProjectInstructions(dir string) (name, block string) {
	name = findAgentsFile(dir)
	if name == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", ""
	}
	content := strings.TrimSpace(string(raw))
	if content == "" {
		return "", ""
	}
	truncated := ""
	if len(content) > projectInstructionsCap {
		content = content[:projectInstructionsCap]
		truncated = "\n[truncated]"
	}
	block = fmt.Sprintf("\n\nThe project provides instructions in %s (below). Follow them where they apply.\n\n--- %s ---\n%s%s\n--- end of %s ---",
		name, name, content, truncated, name)
	return name, block
}

// findAgentsFile returns the agents.md filename present in dir. Exact casings
// are preferred in a fixed order so the pick is deterministic when a
// case-sensitive filesystem holds more than one variant.
func findAgentsFile(dir string) string {
	for _, want := range []string{"AGENTS.md", "agents.md"} {
		if fi, err := os.Stat(filepath.Join(dir, want)); err == nil && !fi.IsDir() {
			return want
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), "agents.md") {
			return e.Name()
		}
	}
	return ""
}
