package tools

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer makes bytes.Buffer safe for the liveWriter's two writers (the
// output pump and the ticker goroutine).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A quiet command must get a liveness line: after the silence threshold the
// writer draws an elapsed-time heartbeat, and Stop clears it.
func TestLiveWriterShowsHeartbeatWhenSilent(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the real silence threshold")
	}
	buf := &syncBuffer{}
	w := startLiveWriter(buf)
	time.Sleep(liveSilentAfter + 500*time.Millisecond)
	w.Stop()
	out := buf.String()
	if !strings.Contains(out, "running (no output for a while)") {
		t.Fatalf("no liveness line after %v of silence; output %q", liveSilentAfter, out)
	}
	if !strings.HasSuffix(out, "\r\033[K") {
		t.Fatalf("Stop must clear the liveness line; output ends %q", out[max(0, len(out)-20):])
	}
}

// Output passes through untouched, resets the silence clock, and clears any
// liveness line before the chunk lands.
func TestLiveWriterPassesOutputThrough(t *testing.T) {
	buf := &syncBuffer{}
	w := startLiveWriter(buf)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	if got := buf.String(); got != "hello\n" {
		t.Fatalf("chatty output must pass through with no liveness noise; got %q", got)
	}
}

// A partial line (no trailing newline) must suppress the heartbeat: drawing
// with \r would clobber the command's own text.
func TestLiveWriterRespectsPartialLines(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the real silence threshold")
	}
	buf := &syncBuffer{}
	w := startLiveWriter(buf)
	if _, err := w.Write([]byte("progress: 42%")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(liveSilentAfter + 500*time.Millisecond)
	w.Stop()
	if got := buf.String(); got != "progress: 42%" {
		t.Fatalf("heartbeat drew over a partial line; got %q", got)
	}
}
