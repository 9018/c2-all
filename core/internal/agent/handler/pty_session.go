package handler

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
)

// PtySession is one interactive shell session with a pseudo-terminal.
// The session id is the C2 JobID, which is also used to stream output back.
type PtySession struct {
	ID      string
	PTY     *os.File
	Process *os.Process
	Stdin   io.Writer

	mu       sync.Mutex
	closed   atomic.Bool
	closeCh  chan struct{}
	doneOnce sync.Once
}

// PtySessions holds all live interactive sessions keyed by JobID.
var PtySessions sync.Map

// NewPtySession starts a new interactive shell in a PTY.
func NewPtySession(id, shell string, env []string) (*PtySession, error) {
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), env...)
	f, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	s := &PtySession{
		ID:      id,
		PTY:     f,
		Process: cmd.Process,
		Stdin:   f,
		closeCh: make(chan struct{}),
	}
	PtySessions.Store(id, s)
	return s, nil
}

// Read chunks of output from the PTY until the session ends.
func (s *PtySession) StreamOutput(out func([]byte)) {
	buf := make([]byte, 4096)
	for {
		n, err := s.PTY.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			out(chunk)
		}
		if err != nil {
			break
		}
	}
	s.closed.Store(true)
	close(s.closeCh)
}

// WriteInput writes bytes into the PTY master (stdin of the shell).
func (s *PtySession) WriteInput(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return io.EOF
	}
	_, err := s.PTY.Write(data)
	return err
}

// Resize updates the PTY window size (rows x cols).
func (s *PtySession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return io.EOF
	}
	return pty.Setsize(s.PTY, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close terminates the session and cleans up.
func (s *PtySession) Close() {
	s.doneOnce.Do(func() {
		s.closed.Store(true)
		_ = s.PTY.Close()
		if s.Process != nil {
			_ = s.Process.Kill()
			_, _ = s.Process.Wait()
		}
		PtySessions.Delete(s.ID)
	})
}