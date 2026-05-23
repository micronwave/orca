package eventlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

// FileLog is the JSONL-backed EventLog implementation. Each line is one
// JSON-encoded schema.Event terminated by '\n'. Appends are fsynced before
// returning. Read methods open a separate file descriptor; no buffered state
// is shared between the writer and readers. All methods are safe for
// concurrent use.
type FileLog struct {
	mu   sync.RWMutex
	path string
	seq  int64 // last assigned sequence number; 0 means no events yet
	f    *os.File
	err  error
	done bool
}

var (
	ErrClosed          = errors.New("eventlog: closed")
	ErrUncertainCommit = errors.New("eventlog: append sync failed; commit state uncertain")
)

// Open opens or creates the JSONL event log at path.
// It scans any existing entries to initialise the sequence counter so that
// the next Append continues from where the previous process left off.
// Returns an error if any completed line cannot be parsed (corrupt log). A
// malformed unterminated final line is treated as a crash-partial append and
// truncated before opening for append.
//
// Only one OS process may write to the same path at a time. Open does not
// acquire an inter-process lock; two concurrent writers would produce
// duplicate SequenceNum values, corrupting the authoritative history.
func Open(path string) (*FileLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: mkdir %s: %w", filepath.Dir(path), err)
	}
	maxSeq, repairAt, err := scanMaxSeq(path)
	if err != nil {
		return nil, err
	}
	if repairAt >= 0 {
		if err := os.Truncate(path, repairAt); err != nil {
			return nil, fmt.Errorf("eventlog: truncate partial final line: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open for append: %w", err)
	}
	return &FileLog{path: path, seq: maxSeq, f: f}, nil
}

// scanMaxSeq reads path and returns the highest SequenceNum found.
// Returns 0 if the file does not exist or is empty.
// Returns an error if any completed line cannot be parsed. A malformed final
// unterminated line is treated as a crash-partial append and returned as a
// truncation point for Open to repair.
func scanMaxSeq(path string) (maxSeq int64, repairAt int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, -1, nil
	}
	if err != nil {
		return 0, -1, fmt.Errorf("eventlog: open for scan: %w", err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	lineNum := 0
	for {
		lineStart := offset
		line, readErr := r.ReadBytes('\n')
		offset += int64(len(line))
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		lineNum++
		if len(line) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return 0, -1, fmt.Errorf("eventlog: scan: %w", readErr)
			}
			break
		}
		terminated := line[len(line)-1] == '\n'
		line = bytesTrimRightNewline(line)
		if len(line) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return 0, -1, fmt.Errorf("eventlog: scan: %w", readErr)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}

		var e schema.Event
		if err := json.Unmarshal(line, &e); err != nil {
			if !terminated && errors.Is(readErr, io.EOF) {
				return maxSeq, lineStart, nil
			}
			return 0, -1, fmt.Errorf("eventlog: corrupt line %d: %w", lineNum, err)
		}
		// A valid-JSON record that is not newline-terminated is an incomplete
		// append (e.g. crash after write, before the '\n' was flushed). The next
		// Append would concatenate JSON directly onto it, corrupting the log.
		if !terminated && errors.Is(readErr, io.EOF) {
			return maxSeq, lineStart, nil
		}
		if e.SequenceNum != maxSeq+1 {
			return 0, -1, fmt.Errorf("eventlog: ordering violation at line %d: got seq %d, want %d", lineNum, e.SequenceNum, maxSeq+1)
		}
		maxSeq = e.SequenceNum
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return 0, -1, fmt.Errorf("eventlog: scan: %w", readErr)
		}
	}
	return maxSeq, -1, nil
}

func bytesTrimRightNewline(line []byte) []byte {
	line = bytesTrimSuffix(line, '\n')
	line = bytesTrimSuffix(line, '\r')
	return line
}

func bytesTrimSuffix(line []byte, suffix byte) []byte {
	if len(line) > 0 && line[len(line)-1] == suffix {
		return line[:len(line)-1]
	}
	return line
}

// Close syncs and closes the underlying file. FileLog must not be used after Close.
func (l *FileLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		_ = l.f.Close()
		l.done = true
		return fmt.Errorf("eventlog: sync on close: %w", err)
	}
	l.done = true
	return l.f.Close()
}

// Append implements EventLog. SequenceNum is assigned monotonically by the
// log; callers must leave it zero. EventID is generated if empty. CreatedAt
// is set to UTC now if zero. The file is fsynced before returning.
func (l *FileLog) Append(ctx context.Context, e schema.Event) (schema.Event, error) {
	if err := ctx.Err(); err != nil {
		return schema.Event{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return schema.Event{}, err
	}
	if l.done {
		return schema.Event{}, ErrClosed
	}
	if l.err != nil {
		return schema.Event{}, l.err
	}

	l.seq++
	e.SequenceNum = l.seq
	if e.EventID == "" {
		e.EventID = newEventID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}

	line, err := json.Marshal(e)
	if err != nil {
		l.seq-- // marshal failed before any write
		return schema.Event{}, fmt.Errorf("eventlog: marshal: %w", err)
	}
	line = append(line, '\n')

	n, err := l.f.Write(line)
	if err != nil {
		if n > 0 {
			// Partial write: bytes may be on disk. Cannot reuse seq safely;
			// poison the log so callers do not continue on corrupt state.
			l.err = fmt.Errorf("%w: partial write (%d/%d bytes): %v", ErrUncertainCommit, n, len(line), err)
			return schema.Event{}, l.err
		}
		l.seq-- // zero bytes written; seq is safe to reuse
		return schema.Event{}, fmt.Errorf("eventlog: write: %w", err)
	}
	if n != len(line) {
		l.err = fmt.Errorf("%w: wrote %d of %d bytes", io.ErrShortWrite, n, len(line))
		return schema.Event{}, l.err
	}
	// Write succeeded. Sequence number is committed even if Sync fails;
	// the OS may flush the buffer independently, so rolling back seq would
	// risk re-using a number that is already on disk.
	if err := l.f.Sync(); err != nil {
		l.err = fmt.Errorf("%w: %v", ErrUncertainCommit, err)
		return schema.Event{}, l.err
	}
	return e, nil
}

// ReadAfter implements EventLog.
func (l *FileLog) ReadAfter(ctx context.Context, afterSeq int64, limit int) ([]schema.Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.done {
		return nil, ErrClosed
	}
	if l.err != nil {
		return nil, l.err
	}
	return l.scan(ctx, afterSeq, limit, func(_ schema.Event) bool { return true })
}

// ReadByType implements EventLog.
func (l *FileLog) ReadByType(ctx context.Context, eventType schema.EventType, afterSeq int64, limit int) ([]schema.Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.done {
		return nil, ErrClosed
	}
	if l.err != nil {
		return nil, l.err
	}
	return l.scan(ctx, afterSeq, limit, func(e schema.Event) bool { return e.Type == eventType })
}

// ReadForGoal implements EventLog.
func (l *FileLog) ReadForGoal(ctx context.Context, goalID string, afterSeq int64, limit int) ([]schema.Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.done {
		return nil, ErrClosed
	}
	if l.err != nil {
		return nil, l.err
	}
	return l.scan(ctx, afterSeq, limit, func(e schema.Event) bool { return e.GoalID == goalID })
}

// scan reads the JSONL file, skipping events with SequenceNum <= afterSeq,
// collecting up to limit events (0 = no limit) that satisfy pred.
// Caller must hold at least l.mu.RLock().
func (l *FileLog) scan(ctx context.Context, afterSeq int64, limit int, pred func(schema.Event) bool) ([]schema.Event, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eventlog: open for read: %w", err)
	}
	defer f.Close()

	var out []schema.Event
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := r.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		if len(line) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("eventlog: scan read: %w", readErr)
			}
			break
		}
		line = bytesTrimRightNewline(line)
		if len(line) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("eventlog: scan read: %w", readErr)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		var e schema.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("eventlog: parse event: %w", err)
		}
		if e.SequenceNum <= afterSeq {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("eventlog: scan read: %w", readErr)
			}
			continue
		}
		if !pred(e) {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("eventlog: scan read: %w", readErr)
			}
			continue
		}
		out = append(out, e)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("eventlog: scan read: %w", readErr)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// newEventID returns a random UUID v4 using crypto/rand.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("eventlog: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// compile-time interface satisfaction check
var _ EventLog = (*FileLog)(nil)
