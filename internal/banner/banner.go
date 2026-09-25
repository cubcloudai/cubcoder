// Package banner renders the one-time startup splash: the CyberBear mascot in
// truecolor half-block ANSI art, with the app name and version centered beneath
// it. The art is pre-rendered (see cyberbear.ans) and embedded so the binary
// stays a single self-contained file with no asset directory to ship.
package banner

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed cyberbear.ans
var bear string

// artWidth is the rendered width of the mascot in terminal columns. The art is
// generated at this fixed width, so the title line below is centered to match.
const artWidth = 34

const (
	orange = "\033[38;2;240;125;0m" // CubCloud brand orange #F07D00
	dim    = "\033[2m"
	reset  = "\033[0m"
)

// Supported reports whether the terminal can render the truecolor half-block
// art. The art needs 24-bit color and Unicode block glyphs, which bare consoles
// lack: the Linux framebuffer console (TERM=linux) caps at 16 colors and draws
// the blocks as garbage, and dumb/empty terminals have no color at all. We treat
// those as unsupported and let the caller fall back to the plain line. Anything
// advertising truecolor (COLORTERM) or a 256-color/xterm-class TERM is fine.
func Supported() bool {
	switch os.Getenv("TERM") {
	case "", "dumb", "linux", "console", "vt100", "vt220":
		return false
	}
	if c := os.Getenv("COLORTERM"); strings.Contains(c, "truecolor") || strings.Contains(c, "24bit") {
		return true
	}
	term := os.Getenv("TERM")
	return strings.Contains(term, "256color") ||
		strings.HasPrefix(term, "xterm") ||
		strings.HasPrefix(term, "screen") ||
		strings.HasPrefix(term, "tmux")
}

// Splash returns the full startup banner: the mascot followed by a centered
// "name version" wordmark. The trailing newline lets the caller print it as-is.
func Splash(name, version string) string {
	title := orange + name + reset + " " + dim + version + reset
	// Center on artWidth using the visible length (name + space + version),
	// ignoring the ANSI escapes which take no columns.
	visible := len(name) + 1 + len(version)
	pad := (artWidth - visible) / 2
	if pad < 0 {
		pad = 0
	}
	return fmt.Sprintf("%s\n\n%s%s\n", strings.TrimRight(bear, "\n"), strings.Repeat(" ", pad), title)
}
