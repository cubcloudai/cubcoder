package main

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

// escWatcher listens on the controlling terminal while an agent turn runs and
// cancels the turn's context on a bare Escape keypress — the "stop what you're
// doing but keep the session" counterpart to Ctrl-C's exit. It exists because
// nothing else reads the terminal during a run: without it, Esc would just sit
// in the kernel's input queue until the next prompt.
//
// It opens its own /dev/tty descriptor rather than sharing os.Stdin, for the
// same reason the interactive command pump does: O_NONBLOCK registers the fd
// with the runtime poller, so a blocked Read can be woken by a read deadline
// and by Close. That is what lets Pause and Stop take the watcher off the
// terminal immediately, instead of it lingering to swallow the first
// keystroke meant for someone else.
//
// Everything else typed during a run is kept, not discarded: bytes go into a
// stash that Pause/Stop hand back to the caller for re-injection into the
// line reader, so type-ahead still works the way it does without the watcher.
type escWatcher struct {
	tty    *os.File
	cancel func()

	mu     sync.Mutex
	paused bool
	resume chan struct{}
	stash  []byte

	done chan struct{}
}

// escSeqWait is how long a lone ESC byte may sit with no follow-up before it
// counts as the Escape key. Arrow keys and other sequences arrive as a single
// burst (ESC [ A within a few ms), so anything slower is a human pressing Esc.
const escSeqWait = 75 * time.Millisecond

// startEscWatcher begins watching the terminal and returns the watcher, or
// nil when there is no controlling terminal to watch (piped input, one-shot
// runs off a tty) — callers treat nil as "feature unavailable".
func startEscWatcher(cancel func()) *escWatcher {
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	w := &escWatcher{tty: tty, cancel: cancel, done: make(chan struct{})}
	go w.loop()
	return w
}

func (w *escWatcher) loop() {
	defer close(w.done)
	buf := make([]byte, 1)
	for {
		_ = w.tty.SetReadDeadline(time.Time{}) // block until a key or a wake-up
		n, err := w.tty.Read(buf)
		if err != nil {
			if !w.waitIfPaused(err) {
				return // closed: Stop was called
			}
			continue
		}
		if n == 0 {
			continue
		}
		if buf[0] != 0x1b {
			w.stashBytes(buf[:n])
			continue
		}
		// ESC seen: bare Escape, or the first byte of a key sequence? Wait
		// briefly for a continuation; silence means the Escape key.
		_ = w.tty.SetReadDeadline(time.Now().Add(escSeqWait))
		n, err = w.tty.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				w.cancel()
				continue
			}
			if !w.waitIfPaused(err) {
				return
			}
			continue
		}
		if n == 0 {
			w.cancel()
			continue
		}
		// A second ESC inside the window is a human double-tapping the key —
		// no terminal sequence starts ESC ESC. Without this, an impatient
		// double-press would read as a sequence and be dropped entirely.
		if buf[0] == 0x1b {
			w.cancel()
			continue
		}
		// A continuation arrived: this is an escape sequence (arrow key, paste
		// marker, …), not a cancel. Drain the rest of the burst and drop it —
		// replaying half a sequence into the next prompt would type garbage.
		for {
			_ = w.tty.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			n, err = w.tty.Read(buf)
			if n == 0 || err != nil {
				break
			}
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			if !w.waitIfPaused(err) {
				return
			}
		}
	}
}

func (w *escWatcher) stashBytes(b []byte) {
	w.mu.Lock()
	w.stash = append(w.stash, b...)
	w.mu.Unlock()
}

// waitIfPaused blocks until Resume when the read error was a Pause waking the
// loop. It reports whether the loop should keep running: false means the tty
// was closed and the watcher is done.
func (w *escWatcher) waitIfPaused(err error) bool {
	w.mu.Lock()
	ch := w.resume
	paused := w.paused
	w.mu.Unlock()
	if paused && ch != nil {
		<-ch
		return true
	}
	// A deadline wake with no pause pending is benign (Pause/Resume raced the
	// read); anything else — notably file-already-closed from Stop — is final.
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// Pause takes the watcher off the terminal so someone else can read it (the
// tool-approval prompt, an interactive command). It returns the type-ahead
// stashed so far, for injection into whatever reads next. Nil-safe.
func (w *escWatcher) Pause() []byte {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.paused = true
	if w.resume == nil {
		w.resume = make(chan struct{})
	}
	s := w.stash
	w.stash = nil
	w.mu.Unlock()
	_ = w.tty.SetReadDeadline(time.Now()) // wake the blocked read
	return s
}

// Resume puts a paused watcher back on the terminal. Nil-safe.
func (w *escWatcher) Resume() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.paused = false
	ch := w.resume
	w.resume = nil
	w.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// Stop ends the watch and returns any remaining type-ahead. It waits for the
// reader goroutine to exit, so after Stop returns the terminal has exactly one
// reader again. Nil-safe.
func (w *escWatcher) Stop() []byte {
	if w == nil {
		return nil
	}
	_ = w.tty.SetReadDeadline(time.Now())
	_ = w.tty.Close()
	w.mu.Lock()
	w.paused = false
	if w.resume != nil {
		close(w.resume)
		w.resume = nil
	}
	w.mu.Unlock()
	<-w.done
	w.mu.Lock()
	s := w.stash
	w.stash = nil
	w.mu.Unlock()
	return s
}
