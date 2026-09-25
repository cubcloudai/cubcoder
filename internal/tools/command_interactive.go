package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// runInteractive runs the command on a pseudo-terminal wired to the user's
// real terminal, so a command that prompts for input — most commonly a sudo or
// ssh password — can be answered by the user. The command's output streams live
// to con.Out (so the prompt is visible) and is captured in parallel for the
// model.
//
// A pty is what makes the prompt work: pty.Start puts the child in its own
// session with the pty as its controlling terminal, so it is the foreground
// process group of that pty and its reads from the terminal do not raise SIGTTIN
// (which would stop a backgrounded child reading our real terminal and hang the
// command). We then bridge bytes both ways. If there is no controlling terminal
// to read input from, this falls back to the captured, non-interactive run.
//
// A sudoWatch sits on both pumps: it spots sudo's password prompt in the
// output, answers it from the session cache when one is remembered, and
// otherwise captures what the user types so the password can be offered for
// remembering once the command succeeds.
func runInteractive(ctx context.Context, root, command string, con *Console, timeout time.Duration) (string, error) {
	// A fresh handle on the controlling terminal for input. Opening /dev/tty
	// (rather than reusing os.Stdin) gives us a descriptor we fully own: we can
	// unblock and close it the instant the command exits, so the input pump never
	// lingers to swallow the user's next keystroke at the REPL prompt.
	//
	// O_NONBLOCK is essential: it makes Go register the fd with the runtime
	// poller, so the input pump's blocked Read can be interrupted by a read
	// deadline (and by Close). Without it the Read is uninterruptible and the
	// <-inDone wait below would hang the whole REPL after the command finishes.
	tin, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return runCaptured(ctx, root, command, timeout)
	}
	if con.SuspendInput != nil {
		resume := con.SuspendInput()
		defer resume()
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = root
	cmd.Env = commandEnv()
	// pty.Start adds Setsid+Setctty, so the child's process group id equals its
	// pid; kill the whole group on timeout, matching runCaptured's behavior.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = commandWaitDelay

	ptmx, err := pty.Start(cmd)
	if err != nil {
		tin.Close()
		return runCaptured(ctx, root, command, timeout)
	}
	_ = pty.InheritSize(ptmx, tin) // size the pty to the real terminal

	watch := &sudoWatch{con: con, answer: ptmx}

	// A liveness line for quiet stretches: the agent's spinner is suppressed for
	// interactive commands (the command owns the terminal line), so a long
	// silent command — a compile, a download — would otherwise look frozen.
	live := startLiveWriter(con.Out)

	// Pump the user's keystrokes (the typed password) into the command.
	inDone := make(chan struct{})
	go func() {
		watch.pumpInput(ptmx, tin, con.Interrupt)
		close(inDone)
	}()

	// Mirror the command's output to the terminal while capturing it. This
	// returns when the child exits and the pty master reports EOF/EIO, so it is
	// also where we block until the command finishes. The terminal mirror goes
	// through the liveness writer; the model's capture stays raw.
	var buf bytes.Buffer
	watch.pumpOutput(io.MultiWriter(live, &buf), ptmx)
	live.Stop()

	err = cmd.Wait()

	// Stop the input pump before returning control to the REPL: the read deadline
	// wakes a poll-managed Read, and the Close is the reliable backstop.
	_ = tin.SetReadDeadline(time.Now())
	tin.Close()
	<-inDone
	ptmx.Close()

	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	_ = err
	watch.finish(code)
	return commandResult(ctx, code, buf.String(), timeout), nil
}

// liveSilentAfter is how long an interactive command may go without output
// before the liveness line appears. Chatty commands never show it; a quiet
// compile or download gets a heartbeat instead of a frozen terminal.
const liveSilentAfter = 2 * time.Second

// liveWriter sits between an interactive command's output and the terminal.
// It passes output through untouched and, when the command goes quiet, draws
// an in-place elapsed-time line so a long silent stretch doesn't read as a
// hang. The line is drawn only while the cursor sits at the start of a row
// (the last output byte was a newline, or nothing has been printed yet):
// drawing over a partial line would clobber it, and a command animating its
// own progress with \r counts as output and resets the silence clock anyway.
type liveWriter struct {
	mu    sync.Mutex
	out   io.Writer
	atBOL bool      // cursor at the beginning of a line: safe to draw and clear
	shown bool      // a liveness line is currently on screen
	last  time.Time // when command output last arrived
	stop  chan struct{}
	done  chan struct{}
}

func startLiveWriter(out io.Writer) *liveWriter {
	w := &liveWriter{
		out:   out,
		atBOL: true,
		last:  time.Now(),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go w.tick()
	return w
}

// Write mirrors a command output chunk to the terminal, clearing any liveness
// line first so the chunk lands where the command expects the cursor.
func (w *liveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > 0 {
		w.clearLocked()
		w.last = time.Now()
		w.atBOL = p[len(p)-1] == '\n'
	}
	return w.out.Write(p)
}

// clearLocked erases the liveness line if one is on screen. Caller holds mu.
func (w *liveWriter) clearLocked() {
	if w.shown {
		fmt.Fprint(w.out, "\r\033[K")
		w.shown = false
	}
}

func (w *liveWriter) tick() {
	defer close(w.done)
	// ASCII frames only, matching the REPL spinner: braille frames render as
	// identical tofu boxes on fonts without U+28xx coverage.
	frames := `|/-\`
	start := time.Now()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-w.stop:
			w.mu.Lock()
			w.clearLocked()
			w.mu.Unlock()
			return
		case <-t.C:
			w.mu.Lock()
			if w.atBOL && time.Since(w.last) >= liveSilentAfter {
				// Frame and elapsed stay at normal intensity — the dim label can be
				// near-invisible on dark schemes and this is the only liveness signal.
				fmt.Fprintf(w.out, "\r%c \033[2mrunning (no output for a while)…\033[0m %s",
					frames[i%len(frames)], time.Since(start).Round(time.Second))
				w.shown = true
			}
			w.mu.Unlock()
		}
	}
}

// Stop clears any liveness line and joins the ticker goroutine, so after Stop
// returns nothing else writes to the terminal.
func (w *liveWriter) Stop() {
	close(w.stop)
	<-w.done
}

// sudoPromptRe matches sudo's default password prompt at the tail of the
// output stream. It is deliberately narrow: ssh's "user@host's password:" must
// NOT match — caching an ssh password has different security implications and
// stays out of scope.
var sudoPromptRe = regexp.MustCompile(`(?i)\[sudo\] password for [^\n:]*: *$`)

// promptTailMax bounds the rolling output tail the prompt regex runs over.
const promptTailMax = 512

// sudoWatch observes one interactive command's pty traffic for sudo password
// prompts. When the session has a remembered password it answers the prompt
// itself; otherwise it captures the user's keystrokes between the prompt and
// Enter (echo is off, so those bytes appear nowhere in the output — and thus
// never reach the model) so finish can offer to remember them.
type sudoWatch struct {
	con    *Console
	answer io.Writer // the pty master; where an auto-answer is written

	mu        sync.Mutex
	tail      []byte // rolling window of recent output for prompt detection
	capturing bool   // between a manual prompt and the user's Enter
	typed     []byte // password being typed
	captured  []byte // last completed manual entry
	autoSent  bool   // the previous prompt was answered from the cache
}

// pumpOutput mirrors the command's output to dst while scanning it for sudo
// prompts. Returns when src (the pty master) reports EOF/EIO.
func (w *sudoWatch) pumpOutput(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			w.scan(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// inputEscSeqWait mirrors the REPL Esc watcher's heuristic: a lone ESC byte
// with no follow-up within this window is the Escape key; arrow keys and other
// sequences arrive as a single burst within a few milliseconds.
const inputEscSeqWait = 75 * time.Millisecond

// pumpInput forwards the user's keystrokes to dst (the pty master) while
// letting the watch observe them for password capture. A bare Escape keypress
// is not forwarded: it calls interrupt (the turn's cancel) instead, so Esc
// keeps meaning "stop the turn" while an interactive command owns the
// terminal. Escape sequences (arrow keys, function keys) pass through intact.
// This deliberately includes mid password entry at a sudo/ssh prompt — Esc
// there kills the command and the turn, it does not reach the prompt (which
// would have ignored it anyway).
func (w *sudoWatch) pumpInput(dst io.Writer, src *os.File, interrupt func()) {
	buf := make([]byte, 1024)
	for {
		_ = src.SetReadDeadline(time.Time{}) // block until a key, Close, or a probe below
		n, err := src.Read(buf)
		// A fast double-tap can coalesce into one ESC ESC read (the watcher is
		// immune — it reads a byte at a time). All-ESC chunks are a human.
		if interrupt != nil && n > 1 && bytes.Count(buf[:n], []byte{0x1b}) == n {
			interrupt()
			continue
		}
		if n == 1 && buf[0] == 0x1b && interrupt != nil {
			// Bare Escape, or the first byte of a key sequence? Wait briefly for
			// a continuation; silence means the human pressed Esc. A second ESC
			// inside the window is a double-tap — same intent, not a sequence.
			_ = src.SetReadDeadline(time.Now().Add(inputEscSeqWait))
			m, err2 := src.Read(buf[1:])
			if (m == 0 && errors.Is(err2, os.ErrDeadlineExceeded)) || (m > 0 && buf[1] == 0x1b) {
				interrupt()
				continue
			}
			n += m
			err = err2
		}
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			w.observe(buf[:n])
		}
		// A deadline wake is benign (a probe above, or the shutdown nudge that
		// precedes Close); Close itself ends the pump.
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
	}
}

// scan appends an output chunk to the rolling tail and reacts when the tail
// ends in a sudo password prompt: answer from the session cache if possible,
// otherwise start capturing the user's manual entry. A prompt that reappears
// right after an auto-answer means the cached password was rejected — drop it
// silently (sudo's own "Sorry, try again." is already on screen) and fall back
// to manual entry.
func (w *sudoWatch) scan(chunk []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tail = append(w.tail, chunk...)
	if len(w.tail) > promptTailMax {
		w.tail = w.tail[len(w.tail)-promptTailMax:]
	}
	if !sudoPromptRe.Match(w.tail) {
		return
	}
	w.tail = w.tail[:0] // consume, so one prompt fires once

	if w.autoSent {
		w.con.ForgetSudo()
		w.autoSent = false
	} else if pw, ok := w.con.sudoPassword(); ok {
		w.autoSent = true
		_, _ = w.answer.Write(append(pw, '\n'))
		fmt.Fprint(w.con.Out, "(using remembered sudo password)")
		return
	}
	w.capturing = true
	w.typed = w.typed[:0]
}

// observe watches the user's keystrokes while a manual password entry is in
// progress. The pty slave is in canonical mode with echo off, so the bytes here
// are the raw line: printable characters, backspace/kill editing, and the Enter
// that submits. Ctrl-C aborts the capture.
func (w *sudoWatch) observe(keys []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.capturing {
		return
	}
	for _, b := range keys {
		switch b {
		case '\r', '\n':
			w.capturing = false
			if len(w.typed) > 0 {
				w.captured = append(w.captured[:0:0], w.typed...)
			}
			w.typed = w.typed[:0]
			return
		case 0x7f, 0x08: // backspace / delete
			if len(w.typed) > 0 {
				w.typed = w.typed[:len(w.typed)-1]
			}
		case 0x15: // Ctrl-U (kill line)
			w.typed = w.typed[:0]
		case 0x03: // Ctrl-C — abort
			w.capturing = false
			w.typed = w.typed[:0]
			return
		default:
			if b >= 0x20 { // printable ASCII and UTF-8 continuation bytes
				w.typed = append(w.typed, b)
			}
		}
	}
}

// finish runs after the command exits. If the user typed a sudo password, the
// command succeeded, and no password is already remembered, offer to remember
// it for the rest of the session. The password lives in memory only and is
// never written to disk or shown to the model.
func (w *sudoWatch) finish(code int) {
	w.mu.Lock()
	pw := w.captured
	w.captured = nil
	w.mu.Unlock()
	if len(pw) == 0 || code != 0 || w.con.Ask == nil || w.con.SudoRemembered() {
		return
	}
	if w.con.Ask("remember sudo password for this session? (memory only, cleared on exit)") {
		w.con.rememberSudo(pw)
		fmt.Fprintln(w.con.Out, "(sudo password remembered — /sudo forget to clear)")
	}
}
