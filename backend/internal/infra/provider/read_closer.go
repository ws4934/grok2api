package provider

import (
	"errors"
	"io"
	"sync"
)

// NewCompletionReadCloser reports whether a streamed body reached EOF before
// it was closed. Callers can record success only after a complete transfer,
// while still observing transport and close errors exactly once.
func NewCompletionReadCloser(body io.ReadCloser, onFinished func(error, bool)) io.ReadCloser {
	return &completionReadCloser{ReadCloser: body, onFinished: onFinished}
}

type completionReadCloser struct {
	io.ReadCloser
	onFinished func(error, bool)
	once       sync.Once
	readErr    error
	complete   bool
}

func (c *completionReadCloser) Read(buffer []byte) (int, error) {
	read, err := c.ReadCloser.Read(buffer)
	if errors.Is(err, io.EOF) {
		c.complete = true
	} else if err != nil {
		c.readErr = err
	}
	return read, err
}

func (c *completionReadCloser) Close() error {
	closeErr := c.ReadCloser.Close()
	c.once.Do(func() {
		feedbackErr := c.readErr
		if feedbackErr == nil {
			feedbackErr = closeErr
		}
		if c.onFinished != nil {
			c.onFinished(feedbackErr, c.complete)
		}
	})
	return closeErr
}

// StreamPipeReadCloser is the read side of a pipe fed by a goroutine copying
// from source. Close also closes source, so a copier blocked in source.Read
// returns instead of holding the upstream until its next event.
type StreamPipeReadCloser struct {
	*io.PipeReader
	source    io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func NewStreamPipeReadCloser(reader *io.PipeReader, source io.ReadCloser) *StreamPipeReadCloser {
	return &StreamPipeReadCloser{PipeReader: reader, source: source}
}

func (r *StreamPipeReadCloser) Close() error {
	readerErr := r.PipeReader.Close()
	sourceErr := r.CloseSource()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

// CloseSource closes source once. Copiers defer it instead of source.Close so
// their close never races the downstream Close.
func (r *StreamPipeReadCloser) CloseSource() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.source.Close()
	})
	return r.closeErr
}
