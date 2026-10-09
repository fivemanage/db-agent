package backup

import (
	"context"
	"io"
	"sync"
)

type source struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dump    func(context.Context, io.Writer) error
	reader  *io.PipeReader
	writer  *io.PipeWriter
	mu      sync.Mutex
	closed  bool
	started bool
	done    chan struct{}
	err     error
}

func newSource(ctx context.Context, dump func(context.Context, io.Writer) error) *source {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	return &source{ctx: ctx, cancel: cancel, dump: dump, reader: reader, writer: writer, done: make(chan struct{})}
}

func (s *source) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	// A queued transfer or insufficient spool headroom must not launch a dump.
	if !s.started {
		s.started = true
		go func() {
			s.err = s.dump(s.ctx, s.writer)
			// EOF is only valid after process exit and credential cleanup succeed.
			s.writer.CloseWithError(s.err)
			close(s.done)
		}()
	}
	s.mu.Unlock()

	return s.reader.Read(p)
}

func (s *source) Close() error {
	s.mu.Lock()
	s.closed = true
	started := s.started
	s.mu.Unlock()

	s.cancel()
	s.reader.Close()
	s.writer.Close()
	if started {
		<-s.done
	}
	return nil
}
