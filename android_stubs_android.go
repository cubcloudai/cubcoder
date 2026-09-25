//go:build android

package main

import (
	"syscall"
	"unsafe"
)

// makeRawInput is a no-op on Android; we don't need raw terminal mode for
// the gomobile binding since the UI handles input via EditText.
func makeRawInput(fd uintptr) (func() error, bool) { return nil, false }

// termWidth is unknowable without a TTY; 0 disables wrap tracking.
func termWidth(fd uintptr) int {
	// Try to get window size via ioctl even on Android for potential future
	// terminal use (e.g. in Termux).
	var ws struct{ rows, cols, xpix, ypix uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, 0x40087468 /* TIOCGWINSZ */, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0
	}
	return int(ws.cols)
}

// windowTitle is a no-op on Android (no terminal title bar).
type windowTitle struct{ enabled bool }

func (w *windowTitle) Set(label string) {}

func (w *windowTitle) Clear() {}

func (w *windowTitle) Enabled() bool { return false }
