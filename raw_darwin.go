//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// makeRawInput switches the terminal at fd into a partial raw mode: canonical
// line buffering and echo are turned off (so the reader can handle bracketed
// paste and echo keystrokes itself), but output processing (OPOST) and signal
// generation (ISIG) stay on, so existing print code and Ctrl-C are unchanged.
// It returns a restore func and whether the change succeeded.
func makeRawInput(fd uintptr) (func() error, bool) {
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, false
	}
	n := old
	n.Lflag &^= syscall.ICANON | syscall.ECHO
	// Keep Enter as CR so Ctrl-J (LF) can insert a newline instead of submitting.
	n.Iflag &^= syscall.ICRNL
	n.Cc[syscall.VMIN] = 1
	n.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&n))); e != 0 {
		return nil, false
	}
	return func() error {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&old))); e != 0 {
			return e
		}
		return nil
	}, true
}

// termWidth reports the terminal's column count, or 0 if it can't be read.
// The line editor needs it to know where the terminal soft-wraps input.
func termWidth(fd uintptr) int {
	var ws struct{ rows, cols, xpix, ypix uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0
	}
	return int(ws.cols)
}
