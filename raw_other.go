//go:build !linux && !darwin

package main

// makeRawInput is a no-op on platforms we don't ship raw-mode support for; the
// reader falls back to line-buffered input.
func makeRawInput(fd uintptr) (func() error, bool) { return nil, false }

// termWidth is unknowable without raw-mode support; 0 disables wrap tracking.
func termWidth(fd uintptr) int { return 0 }
