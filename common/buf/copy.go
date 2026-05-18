package buf

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/features/stats"
)

type dataHandler func(MultiBuffer)

type copyHandler struct {
	onData []dataHandler
}

// SizeCounter is for counting bytes copied by Copy().
type SizeCounter struct {
	Size int64
}

// CopyOption is an option for copying data.
type CopyOption func(*copyHandler)

// UpdateActivity is a CopyOption to update activity on each data copy operation.
func UpdateActivity(timer signal.ActivityUpdater) CopyOption {
	return func(handler *copyHandler) {
		handler.onData = append(handler.onData, func(MultiBuffer) {
			timer.Update()
		})
	}
}

// CountSize is a CopyOption that sums the total size of data copied into the given SizeCounter.
func CountSize(sc *SizeCounter) CopyOption {
	return func(handler *copyHandler) {
		handler.onData = append(handler.onData, func(b MultiBuffer) {
			sc.Size += int64(b.Len())
		})
	}
}

// AddToStatCounter a CopyOption add to stat counter
func AddToStatCounter(sc stats.Counter) CopyOption {
	return func(handler *copyHandler) {
		handler.onData = append(handler.onData, func(b MultiBuffer) {
			if sc != nil {
				sc.Add(int64(b.Len()))
			}
		})
	}
}

type readError struct {
	error
}

func (e readError) Error() string {
	return e.error.Error()
}

func (e readError) Unwrap() error {
	return e.error
}

// IsReadError returns true if the error in Copy() comes from reading.
func IsReadError(err error) bool {
	_, ok := err.(readError)
	return ok
}

type writeError struct {
	error
}

func (e writeError) Error() string {
	return e.error.Error()
}

func (e writeError) Unwrap() error {
	return e.error
}

// IsWriteError returns true if the error in Copy() comes from writing.
func IsWriteError(err error) bool {
	_, ok := err.(writeError)
	return ok
}

func copyInternal(reader Reader, writer Writer, handler *copyHandler) error {
	for {
		buffer, err := reader.ReadMultiBuffer()
		if !buffer.IsEmpty() {
			for _, handler := range handler.onData {
				handler(buffer)
			}

			if werr := writer.WriteMultiBuffer(buffer); werr != nil {
				return writeError{werr}
			}
		}

		if err != nil {
			return readError{err}
		}
	}
}

func copyInternalCtx(ctx context.Context, reader Reader, writer Writer, handler *copyHandler) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		buffer, err := reader.ReadMultiBuffer()
		if !buffer.IsEmpty() {
			for _, handler := range handler.onData {
				handler(buffer)
			}

			if werr := writer.WriteMultiBuffer(buffer); werr != nil {
				return writeError{werr}
			}
		}

		if err != nil {
			return readError{err}
		}
	}
}

// Copy dumps all payload from reader to writer or stops when an error occurs. It returns nil when EOF.
func Copy(reader Reader, writer Writer, options ...CopyOption) error {
	var handler copyHandler
	for _, option := range options {
		option(&handler)
	}
	err := copyInternal(reader, writer, &handler)
	if err != nil && errors.Cause(err) != io.EOF {
		return err
	}
	return nil
}

// CopyCtx is like Copy but respects context cancellation.
func CopyCtx(ctx context.Context, reader Reader, writer Writer, options ...CopyOption) error {
	var handler copyHandler
	for _, option := range options {
		option(&handler)
	}
	err := copyInternalCtx(ctx, reader, writer, &handler)
	if err != nil && errors.Cause(err) != io.EOF && err != context.Canceled && err != context.DeadlineExceeded {
		return err
	}
	return nil
}

var ErrNotTimeoutReader = errors.New("not a TimeoutReader")

func CopyOnceTimeout(reader Reader, writer Writer, timeout time.Duration) error {
	timeoutReader, ok := reader.(TimeoutReader)
	if !ok {
		return ErrNotTimeoutReader
	}
	mb, err := timeoutReader.ReadMultiBufferTimeout(timeout)
	if err != nil {
		return err
	}
	return writer.WriteMultiBuffer(mb)
}

// readDeadlineTimeout is the interval at which DeadlineReader wakes up to
// check context cancellation. 30 seconds balances responsiveness (context
// cancel is noticed within 30s) against overhead (one extra syscall every
// 30s of idle time, negligible vs actual I/O).
const readDeadlineTimeout = 30 * time.Second

// DeadlineReader wraps a Reader and a net.Conn so that ReadMultiBuffer
// periodically unblocks via SetReadDeadline. On each timeout it checks
// whether the context has been cancelled; if so it returns the context
// error, otherwise it resets the deadline and retries the read.
//
// This solves the fundamental problem with CopyCtx: the context check
// only runs between loop iterations, but ReadMultiBuffer can block
// forever on a stalled connection, preventing the context from ever
// being observed.
type DeadlineReader struct {
	reader  Reader
	conn    net.Conn
	ctx     context.Context
	timeout time.Duration
}

// NewDeadlineReader creates a DeadlineReader. The conn MUST be the same
// connection that the reader ultimately reads from (or a wrapper around
// it that delegates SetReadDeadline). If conn is nil, the reader is
// returned unwrapped — this is safe for callers that may not always
// have a net.Conn available.
func NewDeadlineReader(ctx context.Context, conn net.Conn, reader Reader) Reader {
	if conn == nil {
		return reader
	}
	return &DeadlineReader{
		reader:  reader,
		conn:    conn,
		ctx:     ctx,
		timeout: readDeadlineTimeout,
	}
}

// SetDeadlineTimeout overrides the internal poll interval. This is intended
// for tests that cannot afford to wait the default 30 seconds.
func (r *DeadlineReader) SetDeadlineTimeout(d time.Duration) {
	r.timeout = d
}

// ReadMultiBuffer implements Reader. It sets a read deadline before each
// read attempt. If the read returns a timeout error, it checks ctx and
// either returns the context error or retries. All other errors
// (including io.EOF) are returned as-is so the caller sees them
// unchanged.
func (r *DeadlineReader) ReadMultiBuffer() (MultiBuffer, error) {
	for {
		// Check context before setting deadline — fast path for already-cancelled.
		select {
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		default:
		}

		r.conn.SetReadDeadline(time.Now().Add(r.timeout))
		mb, err := r.reader.ReadMultiBuffer()

		if err != nil {
			// Distinguish timeout from real errors.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Deadline fired. Check if context wants us to stop.
				select {
				case <-r.ctx.Done():
					return nil, r.ctx.Err()
				default:
					// Context still alive — clear deadline and retry.
					r.conn.SetReadDeadline(time.Time{})
					continue
				}
			}
			// Real error (EOF, connection reset, etc.) — clear deadline and return.
			r.conn.SetReadDeadline(time.Time{})
			return mb, err
		}

		// Successful read — clear deadline so subsequent writes on the same
		// conn (if any) are not affected, then return data.
		r.conn.SetReadDeadline(time.Time{})
		return mb, nil
	}
}
