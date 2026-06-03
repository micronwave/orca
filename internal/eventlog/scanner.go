package eventlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/micronwave/orca/internal/schema"
)

// Scanner reads events from a FileLog using a single open file descriptor.
// It yields events in strict sequence order, starting after the sequence
// number given to [FileLog.NewScanner].
//
// Create with [FileLog.NewScanner], iterate with [Scanner.Next], release
// resources with [Scanner.Close]. Scanner is not safe for concurrent use.
type Scanner struct {
	f       *os.File
	r       *bufio.Reader
	prevSeq int64
	maxSeq  int64 // committed seq at scanner-open time; won't read past it
}

// NewScanner returns a Scanner that yields events with SequenceNum > afterSeq
// up to (and including) the sequence number committed at the moment this
// method is called. Events appended to the log after NewScanner returns are
// not visible to this Scanner.
//
// When afterSeq equals or exceeds the committed sequence, the returned Scanner
// is immediately exhausted: the first Next call returns (_, false, nil).
func (l *FileLog) NewScanner(afterSeq int64) (*Scanner, error) {
	l.mu.RLock()
	maxSeq := l.seq
	done := l.done
	lerr := l.err
	// Capture the seek offset before releasing the lock.
	var seekOffset int64
	useSeek := afterSeq > 0 && afterSeq < maxSeq
	if useSeek {
		// offsets[afterSeq] is the byte offset of the event with
		// SequenceNum = afterSeq+1, since offsets is 0-indexed relative to seq.
		seekOffset = l.offsets[afterSeq]
	}
	l.mu.RUnlock()

	if done {
		return nil, ErrClosed
	}
	if lerr != nil {
		return nil, lerr
	}

	// Nothing to yield.
	if afterSeq >= maxSeq {
		return &Scanner{maxSeq: maxSeq, prevSeq: afterSeq}, nil
	}

	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return &Scanner{maxSeq: maxSeq, prevSeq: afterSeq}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("eventlog: scanner open: %w", err)
	}

	if useSeek {
		if _, err := f.Seek(seekOffset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("eventlog: scanner seek to offset %d: %w", seekOffset, err)
		}
	}

	return &Scanner{
		f:       f,
		r:       bufio.NewReaderSize(f, 1<<20),
		prevSeq: afterSeq,
		maxSeq:  maxSeq,
	}, nil
}

// Next returns the next event in sequence order.
// ok is false when all committed events have been yielded (the Scanner is
// exhausted). err is set on I/O or parse errors.
//
// After Next returns (_, false, nil) or a non-nil error, all subsequent
// calls return (_, false, nil).
func (s *Scanner) Next(ctx context.Context) (schema.Event, bool, error) {
	if s.f == nil || s.prevSeq >= s.maxSeq {
		return schema.Event{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return schema.Event{}, false, err
	}

	for {
		line, readErr := s.r.ReadBytes('\n')
		if len(line) == 0 {
			if errors.Is(readErr, io.EOF) {
				return schema.Event{}, false, nil
			}
			if readErr != nil {
				return schema.Event{}, false, fmt.Errorf("eventlog: scanner read: %w", readErr)
			}
			continue
		}
		terminated := line[len(line)-1] == '\n'
		line = bytesTrimRightNewline(line)
		if len(line) == 0 {
			if errors.Is(readErr, io.EOF) {
				return schema.Event{}, false, nil
			}
			if readErr != nil {
				return schema.Event{}, false, fmt.Errorf("eventlog: scanner read: %w", readErr)
			}
			continue
		}
		// Partial line at EOF is an in-progress write; stop cleanly.
		if !terminated && errors.Is(readErr, io.EOF) {
			return schema.Event{}, false, nil
		}

		var e schema.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return schema.Event{}, false, fmt.Errorf("eventlog: scanner parse: %w", err)
		}
		if e.SequenceNum != s.prevSeq+1 {
			return schema.Event{}, false, fmt.Errorf("eventlog: scanner ordering violation: got seq %d after seq %d", e.SequenceNum, s.prevSeq)
		}
		s.prevSeq = e.SequenceNum
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return schema.Event{}, false, fmt.Errorf("eventlog: scanner read: %w", readErr)
		}
		return e, true, nil
	}
}

// Close releases the underlying file descriptor.
// Close is idempotent; calling it on an already-closed or empty Scanner is safe.
func (s *Scanner) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
