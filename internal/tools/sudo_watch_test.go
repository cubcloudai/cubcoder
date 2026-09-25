package tools

import (
	"bytes"
	"io"
	"testing"
)

func TestSudoPromptRe(t *testing.T) {
	matches := []string{
		"[sudo] password for user: ",
		"[sudo] password for user:",
		"Sorry, try again.\n[sudo] password for root: ",
	}
	for _, s := range matches {
		if !sudoPromptRe.MatchString(s) {
			t.Errorf("expected match: %q", s)
		}
	}
	nonMatches := []string{
		"user@example.com's password: ",             // ssh — must not match
		"Password: ",                                 // too generic
		"[sudo] password for user: granted\ndone", // prompt not at tail
	}
	for _, s := range nonMatches {
		if sudoPromptRe.MatchString(s) {
			t.Errorf("expected no match: %q", s)
		}
	}
}

// A manual entry — with a typo corrected by backspace — is captured and, after
// a successful exit and a yes answer, remembered on the Console.
func TestSudoWatchCaptureAndRemember(t *testing.T) {
	asked := ""
	con := &Console{Out: io.Discard, Ask: func(q string) bool { asked = q; return true }}
	w := &sudoWatch{con: con, answer: io.Discard}

	w.scan([]byte("[sudo] password for user: "))
	w.observe([]byte("hunter3"))
	w.observe([]byte{0x7f}) // backspace the 3
	w.observe([]byte("2\r"))
	w.observe([]byte("stray keys after enter")) // must not join the password
	w.finish(0)

	if asked == "" {
		t.Fatal("user was never asked to remember the password")
	}
	pw, ok := con.sudoPassword()
	if !ok || string(pw) != "hunter2" {
		t.Fatalf("remembered %q, want %q", pw, "hunter2")
	}
}

// A failed command must not offer to remember what was typed.
func TestSudoWatchDiscardOnFailure(t *testing.T) {
	con := &Console{Out: io.Discard, Ask: func(string) bool {
		t.Fatal("must not ask after a failed command")
		return false
	}}
	w := &sudoWatch{con: con, answer: io.Discard}
	w.scan([]byte("[sudo] password for user: "))
	w.observe([]byte("hunter2\r"))
	w.finish(1)
	if con.SudoRemembered() {
		t.Fatal("password remembered despite failure")
	}
}

// A remembered password answers the prompt automatically; a re-prompt right
// after (wrong password) drops the cache and falls back to manual capture.
func TestSudoWatchAutoAnswerAndRejection(t *testing.T) {
	con := &Console{Out: io.Discard}
	con.rememberSudo([]byte("hunter2"))
	var answered bytes.Buffer
	w := &sudoWatch{con: con, answer: &answered}

	w.scan([]byte("[sudo] password for user: "))
	if got := answered.String(); got != "hunter2\n" {
		t.Fatalf("auto-answer wrote %q, want %q", got, "hunter2\n")
	}
	if w.capturing {
		t.Fatal("must not capture keystrokes while auto-answering")
	}

	// sudo rejects it and prompts again
	w.scan([]byte("Sorry, try again.\n[sudo] password for user: "))
	if con.SudoRemembered() {
		t.Fatal("rejected password still cached")
	}
	if !w.capturing {
		t.Fatal("expected fallback to manual capture after rejection")
	}
}

// A prompt split across two output reads is still detected.
func TestSudoWatchSplitPrompt(t *testing.T) {
	con := &Console{Out: io.Discard}
	w := &sudoWatch{con: con, answer: io.Discard}
	w.scan([]byte("[sudo] password"))
	w.scan([]byte(" for user: "))
	if !w.capturing {
		t.Fatal("split prompt not detected")
	}
}

func TestConsoleForgetSudo(t *testing.T) {
	con := &Console{}
	if con.ForgetSudo() {
		t.Fatal("forgot a password that was never remembered")
	}
	con.rememberSudo([]byte("hunter2"))
	if !con.SudoRemembered() || !con.ForgetSudo() || con.SudoRemembered() {
		t.Fatal("remember/forget cycle broken")
	}
}
