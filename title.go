package main

import (
	"fmt"
	"strings"
)

// windowTitle labels the terminal window via the xterm OSC 0 sequence, which
// sets the window/tab title. The title bar is owned by the terminal emulator,
// not us, but OSC 0 is the one knob every mainstream emulator honors (xterm,
// VTE/GNOME Terminal, Konsole, Kitty, Alacritty, iTerm2, Windows Terminal;
// tmux forwards it when set-titles is on), so a labeled session shows up in
// the title bar, tab strip, taskbar, and Alt-Tab switcher — which is what lets
// the user keep several cubcoder windows ordered.
//
// Before the first Set, the current title is pushed on the terminal's title
// stack (XTWINOPS 22) and Clear pops it back (XTWINOPS 23), so exiting returns
// the window to whatever title the shell had. Emulators without a title stack
// ignore those two sequences and simply keep the last title set.
type windowTitle struct {
	enabled bool // stdout is a terminal
	label   string
	saved   bool // original title pushed on the stack
}

func (w *windowTitle) Set(label string) {
	if !w.enabled {
		return
	}
	if !w.saved {
		fmt.Print("\033[22;0t")
		w.saved = true
	}
	w.label = label
	fmt.Printf("\033]0;%s\a", sanitizeTitle(label))
}

// Clear restores the pre-label title. Idempotent, so it's safe from both the
// deferred exit path and the signal handler.
func (w *windowTitle) Clear() {
	if !w.enabled || !w.saved {
		return
	}
	fmt.Print("\033[23;0t")
	w.label = ""
	w.saved = false
}

// sanitizeTitle drops control characters: a control byte inside an OSC string
// would terminate or corrupt the sequence.
func sanitizeTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
