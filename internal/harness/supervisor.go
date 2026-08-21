package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Defaults for a zero-value Supervisor.
const (
	// DefaultMaxLineBytes caps one output line. Agent CLIs emit whole tool
	// results as single JSON lines, so 64 KiB (bufio.Scanner's default) is far
	// too small; a line past this cap is truncated and flagged, never dropped.
	DefaultMaxLineBytes = 8 << 20

	// DefaultMaxStderrTailBytes is how much stderr a Result carries for
	// diagnostics. Every line still arrives as an event.
	DefaultMaxStderrTailBytes = 32 << 10

	// DefaultDrainGrace is how long the supervisor waits for readers to finish
	// after the process exits, before forcing the pipes closed.
	DefaultDrainGrace = 5 * time.Second

	// DefaultTerminateGrace bounds cleanup of whatever the run left behind.
	DefaultTerminateGrace = 15 * time.Second
)

// Supervisor runs agent CLIs and turns their output into events. The zero value
// with a Launcher set is usable.
type Supervisor struct {
	Launcher Launcher

	// Store records the run. Optional; a nil Store means the caller is doing
	// its own recording.
	Store RunStore

	MaxLineBytes       int
	MaxStderrTailBytes int
	DrainGrace         time.Duration
	TerminateGrace     time.Duration
}

func (s *Supervisor) maxLineBytes() int {
	if s.MaxLineBytes > 0 {
		return s.MaxLineBytes
	}
	return DefaultMaxLineBytes
}

func (s *Supervisor) maxStderrTailBytes() int {
	if s.MaxStderrTailBytes > 0 {
		return s.MaxStderrTailBytes
	}
	return DefaultMaxStderrTailBytes
}

func (s *Supervisor) drainGrace() time.Duration {
	if s.DrainGrace > 0 {
		return s.DrainGrace
	}
	return DefaultDrainGrace
}

func (s *Supervisor) terminateGrace() time.Duration {
	if s.TerminateGrace > 0 {
		return s.TerminateGrace
	}
	return DefaultTerminateGrace
}

// Run executes one agent run to completion.
//
// It returns a Result for every outcome that produced one, including failures,
// so a caller always has something to record. The error is non-nil only when
// the run could not be carried out or the caller's own callback failed; check
// Result.State for how the run itself ended.
func (s *Supervisor) Run(ctx context.Context, spec RunSpec, onEvent EventFunc) (Result, error) {
	if s.Launcher == nil {
		return Result{}, errors.New("harness: Supervisor.Launcher is nil")
	}
	if err := spec.validate(); err != nil {
		return Result{}, err
	}

	startedAt := time.Now()
	result := Result{
		RunID:        spec.RunID,
		State:        StateFailed,
		ExitCode:     -1,
		StartedAt:    startedAt,
		WorkspaceDir: spec.WorkspaceDir,
	}

	if s.Store != nil {
		if err := s.Store.CreateRun(ctx, RunRecord{
			RunID:       spec.RunID,
			Runtime:     spec.Runtime,
			ImageDigest: spec.ImageDigest,
			CLIVersion:  spec.CLIVersion,
			Model:       spec.Model,
			SessionID:   spec.SessionID,
			Network:     spec.Network,
			Limits:      spec.Limits,
			Deadline:    spec.Deadline,
			StartedAt:   startedAt,
		}); err != nil {
			return result, fmt.Errorf("record run: %w", err)
		}
	}

	// The run's own wall clock (D12.6). Enforced here rather than trusted to
	// the CLI, which has every incentive to keep going.
	runCtx, cancelRun := context.WithTimeout(ctx, spec.Deadline)
	defer cancelRun()

	// Cleanup always runs. A container outlives the `podman run` client, so a
	// killed process is not a removed container. Uses a fresh context because
	// runCtx is usually already dead by the time we get here.
	defer func() {
		termCtx, cancelTerm := context.WithTimeout(context.WithoutCancel(ctx), s.terminateGrace())
		defer cancelTerm()
		if err := s.Launcher.Terminate(termCtx, spec); err != nil {
			result.StderrTail = appendTail(result.StderrTail,
				fmt.Sprintf("\n[supervisor] terminate: %v", err), s.maxStderrTailBytes())
		}
	}()

	cmd, err := s.Launcher.Command(runCtx, spec)
	if err != nil {
		result.EndedAt = time.Now()
		return s.finish(ctx, result, fmt.Errorf("build command: %w", err))
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		result.EndedAt = time.Now()
		return s.finish(ctx, result, errors.New("harness: Launcher set Stdout or Stderr; the supervisor owns the pipes"))
	}
	// Escalate from the context's kill if the process ignores it.
	cmd.WaitDelay = s.terminateGrace()

	// os.Pipe rather than cmd.StdoutPipe: exec closes its own pipes inside
	// Wait, which would mean draining before Wait and calling Wait only after
	// draining. A grandchild holding the write end open makes that a deadlock.
	// Owning both ends lets us wait for the process first and force the readers
	// afterwards.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		result.EndedAt = time.Now()
		return s.finish(ctx, result, fmt.Errorf("stdout pipe: %w", err))
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		result.EndedAt = time.Now()
		return s.finish(ctx, result, fmt.Errorf("stderr pipe: %w", err))
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		result.EndedAt = time.Now()
		return s.finish(ctx, result, fmt.Errorf("start: %w", err))
	}

	// The child holds its own copies now. Without closing ours the reader never
	// sees EOF.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	d := &drainer{
		sup:     s,
		ctx:     ctx,
		runID:   spec.RunID,
		onEvent: onEvent,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.consume(stdoutR, StreamStdout)
	}()
	go func() {
		defer wg.Done()
		d.consume(stderrR, StreamStderr)
	}()

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	waitErr := cmd.Wait()

	// The process is gone, but a grandchild may still hold the write end. Give
	// the readers the buffered data, then take the pipes away from them.
	select {
	case <-drained:
	case <-time.After(s.drainGrace()):
		_ = stdoutR.Close()
		_ = stderrR.Close()
		<-drained
	}
	_ = stdoutR.Close()
	_ = stderrR.Close()

	result.EndedAt = time.Now()
	result.EventCount = d.count()
	result.SessionID = d.sessionID()
	if result.SessionID == "" {
		result.SessionID = spec.SessionID
	}
	result.StderrTail = d.stderrTail()
	result.ExitCode = exitCode(waitErr)
	result.State = terminalState(ctx, runCtx, waitErr, d.err())

	return s.finish(ctx, result, d.err())
}

// finish records the terminal state and returns. Recording failures do not
// mask a run error the caller cares more about.
func (s *Supervisor) finish(ctx context.Context, result Result, runErr error) (Result, error) {
	if result.EndedAt.IsZero() {
		result.EndedAt = time.Now()
	}
	if s.Store != nil {
		// Fresh context: the run may have ended because ctx was cancelled, and
		// the terminal state still has to land.
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.terminateGrace())
		defer cancel()
		if err := s.Store.FinishRun(storeCtx, result.RunID, result); err != nil && runErr == nil {
			return result, fmt.Errorf("record terminal state: %w", err)
		}
	}
	return result, runErr
}

// terminalState maps how the process ended onto the vocabulary the rest of the
// platform reasons about.
//
// The caller's context winning yields cancelled even when it expired on its own
// deadline: something above decided to stop, which is different from the run
// outliving the budget it was given.
func terminalState(ctx, runCtx context.Context, waitErr, drainErr error) TerminalState {
	switch {
	case ctx.Err() != nil:
		return StateCancelled
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return StateDeadlineExceeded
	case drainErr != nil:
		return StateFailed
	case waitErr != nil:
		return StateFailed
	default:
		return StateSucceeded
	}
}

func exitCode(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// drainer turns two pipes into one ordered event stream.
//
// The callback is called under a mutex, so a caller never sees two events at
// once and Seq is monotonic. Ordering between stdout and stderr is arrival
// order, which is the only ordering that exists.
type drainer struct {
	sup     *Supervisor
	ctx     context.Context
	runID   string
	onEvent EventFunc

	mu       sync.Mutex
	seq      int
	firstErr error
	session  string
	tail     string
}

func (d *drainer) consume(r io.Reader, stream EventStream) {
	err := scanLines(r, d.sup.maxLineBytes(), func(line string, truncated bool) {
		d.emit(stream, line, truncated)
	})
	// A closed pipe is how the supervisor stops a reader that a grandchild is
	// holding open. That is a deliberate act, not a run failure.
	if err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
		d.fail(fmt.Errorf("read %s: %w", stream, err))
	}
}

func (d *drainer) emit(stream EventStream, line string, truncated bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.seq++
	event := Event{
		Seq:       d.seq,
		At:        time.Now(),
		Stream:    stream,
		Text:      line,
		Truncated: truncated,
	}

	// Only stdout carries stream-json. Parsing stderr would turn a stack trace
	// that happens to start with "{" into a protocol event.
	if stream == StreamStdout && !truncated {
		if envelope, ok := parseStreamJSON(line); ok {
			event.Type = envelope.Type
			event.JSON = envelope.Raw
			if envelope.SessionID != "" {
				d.session = envelope.SessionID
			}
		}
	}

	if stream == StreamStderr {
		d.tail = appendTail(d.tail, line+"\n", d.sup.maxStderrTailBytes())
	}

	// Keep going after the first failure. Returning here would stop reading,
	// the pipe would fill at 64 KiB, and the child would block forever on a
	// write ... which looks exactly like a hung agent.
	if d.firstErr != nil {
		return
	}
	if d.sup.Store != nil {
		if err := d.sup.Store.AppendEvent(d.ctx, d.runID, event); err != nil {
			d.firstErr = fmt.Errorf("record event %d: %w", event.Seq, err)
			return
		}
	}
	if d.onEvent != nil {
		if err := d.onEvent(d.ctx, event); err != nil {
			d.firstErr = fmt.Errorf("event %d: %w", event.Seq, err)
		}
	}
}

func (d *drainer) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.firstErr == nil {
		d.firstErr = err
	}
}

func (d *drainer) err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.firstErr
}

func (d *drainer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seq
}

func (d *drainer) sessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.session
}

func (d *drainer) stderrTail() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tail
}

// streamEnvelope is the part of a stream-json line every runtime agrees on.
type streamEnvelope struct {
	Type      string
	SessionID string
	Raw       json.RawMessage
}

func parseStreamJSON(line string) (streamEnvelope, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return streamEnvelope{}, false
	}

	var probe struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		// opencode and codex have each used camelCase at some point; accept
		// both rather than losing resume across a CLI update.
		SessionIDAlt string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return streamEnvelope{}, false
	}

	sessionID := probe.SessionID
	if sessionID == "" {
		sessionID = probe.SessionIDAlt
	}
	return streamEnvelope{
		Type:      probe.Type,
		SessionID: sessionID,
		Raw:       json.RawMessage(trimmed),
	}, true
}

// scanLines splits r into lines, capping each at maxLine.
//
// bufio.Scanner is the obvious choice and the wrong one: it gives up with
// ErrTooLong on a line past its buffer, which stops the drain and hangs the
// child. This truncates the line, flags it, and keeps reading.
func scanLines(r io.Reader, maxLine int, emit func(line string, truncated bool)) error {
	br := bufio.NewReaderSize(r, 64<<10)

	for {
		var (
			buf       []byte
			truncated bool
			readErr   error
		)

		for {
			chunk, err := br.ReadSlice('\n')
			if len(chunk) > 0 {
				switch {
				case len(buf)+len(chunk) <= maxLine:
					buf = append(buf, chunk...)
				case len(buf) < maxLine:
					buf = append(buf, chunk[:maxLine-len(buf)]...)
					truncated = true
				default:
					truncated = true
				}
			}
			if err == nil {
				break // whole line, delimiter included
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue // long line, keep pulling until the newline
			}
			readErr = err
			break
		}

		if len(buf) > 0 || truncated {
			emit(trimEOL(string(buf)), truncated)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func trimEOL(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// appendTail keeps the last max bytes of a growing string.
func appendTail(tail, add string, max int) string {
	tail += add
	if len(tail) > max {
		tail = tail[len(tail)-max:]
	}
	return tail
}
