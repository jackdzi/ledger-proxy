// Package mux carries many byte streams over one connection (here: the TLS
// connection between the agent on the laptop and edge on the VPS).
//
// Every frame is a 9-byte header followed by a payload:
//
//	type (1 byte) | stream id (4 bytes) | payload length (4 bytes) | payload
//
// Frames:
//
//	OPEN    start a stream; payload = service name ("ledger")
//	DATA    bytes for a stream (at most 32 KB per frame)
//	FIN     the sender won't write more on this stream (half-close, like TCP's FIN)
//	RST     abort the stream
//	WINDOW  flow control: "I've consumed N more bytes, you may send N more"
//	PING    keepalive (stream 0); answered with PONG
//
// Flow control: each side may have at most 1 MB unacknowledged per stream,
// so a slow reader can't make the other side buffer without limit, and one
// big download can't starve the other streams.
//
// Only one side opens streams (edge); the other accepts them (the agent).
package mux

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	frameOpen byte = iota + 1
	frameData
	frameFin
	frameRst
	frameWindow
	framePing
	framePong
)

const (
	headerLen  = 9
	maxPayload = 32 << 10 // per DATA frame
	window     = 1 << 20  // bytes in flight per stream
	pingEvery  = 15 * time.Second
	deadAfter  = 45 * time.Second // no frame at all for this long: the connection is dead
	writeLimit = 30 * time.Second
)

var (
	ErrReset  = errors.New("mux: stream reset by peer")
	ErrClosed = errors.New("mux: session closed")
)

// Session is one multiplexed connection.
type Session struct {
	conn   net.Conn
	out    chan []byte // frames waiting for the writer goroutine
	mu     sync.Mutex
	nextID uint32
	open   map[uint32]*Stream
	accept chan *Stream
	done   chan struct{}
	once   sync.Once
	err    error
}

// New starts a session on conn and begins reading frames and sending pings.
func New(conn net.Conn) *Session {
	s := &Session{conn: conn, out: make(chan []byte, 256), open: map[uint32]*Stream{},
		accept: make(chan *Stream, 64), done: make(chan struct{})}
	go s.readLoop()
	go s.writeLoop()
	go s.pingLoop()
	return s
}

// Open starts a stream to the named service on the other side.
func (s *Session) Open(service string) (*Stream, error) {
	s.mu.Lock()
	if s.isDone() {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.nextID++
	st := newStream(s, s.nextID, service)
	s.open[st.id] = st
	s.mu.Unlock()
	if err := s.writeFrame(frameOpen, st.id, []byte(service)); err != nil {
		return nil, err
	}
	return st, nil
}

// Accept waits for the other side to open a stream.
func (s *Session) Accept() (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, s.Err()
	}
}

// Done is closed when the session ends; Err then says why.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) Close() error { s.fail(ErrClosed); return nil }

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// fail ends the session and every stream on it.
func (s *Session) fail(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		streams := s.open
		s.open = map[uint32]*Stream{}
		s.mu.Unlock()
		s.conn.Close()
		close(s.done)
		for _, st := range streams {
			st.abort(err)
		}
	})
}

// framePool reuses frame buffers, so sending doesn't allocate (and doesn't
// make the garbage collector work) for every frame.
var framePool = sync.Pool{New: func() any { b := make([]byte, 0, headerLen+maxPayload); return &b }}

// writeFrame queues a frame for the writer goroutine. It blocks only while
// the queue is full, which pushes back on fast writers.
func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	bp := framePool.Get().(*[]byte)
	frame := (*bp)[:headerLen+len(payload)]
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:5], id)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[headerLen:], payload)
	select {
	case s.out <- frame:
		return nil
	case <-s.done:
		framePool.Put(bp)
		return s.Err()
	}
}

// writeLoop is the only goroutine that writes to the connection. It takes
// every frame that's already queued, writes them all into one buffer and
// flushes once, so many small frames become a few large TLS records and
// system calls instead of one each.
func (s *Session) writeLoop() {
	w := bufio.NewWriterSize(s.conn, 64<<10)
	for {
		var frame []byte
		select {
		case frame = <-s.out:
		case <-s.done:
			return
		}
		s.conn.SetWriteDeadline(time.Now().Add(writeLimit))
		for {
			_, err := w.Write(frame)
			// The bytes are in w's buffer now, so the frame can be reused.
			// A fresh variable each time: putting &frame itself would let the
			// next loop iteration overwrite what the pool points to.
			done := frame[:0]
			framePool.Put(&done)
			if err != nil {
				s.fail(err)
				return
			}
			select {
			case frame = <-s.out: // more already waiting: batch it
				continue
			default:
			}
			break
		}
		if err := w.Flush(); err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *Session) pingLoop() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.writeFrame(framePing, 0, nil)
		}
	}
}

func (s *Session) stream(id uint32) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open[id]
}

func (s *Session) forget(id uint32) {
	s.mu.Lock()
	delete(s.open, id)
	s.mu.Unlock()
}

func (s *Session) readLoop() {
	r := bufio.NewReaderSize(s.conn, 64<<10)
	var hdr [headerLen]byte
	buf := make([]byte, maxPayload) // reused for every frame: push copies what it keeps
	for {
		s.conn.SetReadDeadline(time.Now().Add(deadAfter)) // any frame proves the peer is alive
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			s.fail(err)
			return
		}
		typ, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:5]), binary.BigEndian.Uint32(hdr[5:9])
		if n > maxPayload {
			s.fail(errors.New("mux: frame too large"))
			return
		}
		payload := buf[:n]
		if _, err := io.ReadFull(r, payload); err != nil {
			s.fail(err)
			return
		}

		switch typ {
		case framePing:
			go s.writeFrame(framePong, 0, nil) // never block the read loop on a write
		case framePong:
			// nothing to do: receiving it already reset the read deadline
		case frameOpen:
			st := newStream(s, id, string(payload))
			s.mu.Lock()
			s.open[id] = st
			s.mu.Unlock()
			select {
			case s.accept <- st:
			default: // nobody is accepting fast enough: refuse
				s.forget(id)
				go s.writeFrame(frameRst, id, nil)
			}
		case frameData:
			if st := s.stream(id); st != nil && !st.push(payload) {
				s.fail(errors.New("mux: peer ignored flow control"))
				return
			}
		case frameFin:
			if st := s.stream(id); st != nil {
				st.remoteFin()
			}
		case frameRst:
			if st := s.stream(id); st != nil {
				st.abort(ErrReset)
			}
		case frameWindow:
			if st := s.stream(id); st != nil && n == 4 {
				st.credit(binary.BigEndian.Uint32(payload))
			}
		default:
			s.fail(errors.New("mux: unknown frame type"))
			return
		}
	}
}

// Stream is one byte stream inside a session. It behaves like a TCP
// connection: Read, Write, CloseWrite (half-close) and Close.
type Stream struct {
	s       *Session
	id      uint32
	service string

	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte // received but not yet read
	store    []byte // the memory buf lives in, reused once buf is empty
	consumed int    // read but not yet acknowledged with WINDOW
	canSend  int    // how many bytes we may still send
	finIn    bool   // the peer finished writing
	finOut   bool   // we finished writing
	closed   bool
	err      error
}

func newStream(s *Session, id uint32, service string) *Stream {
	st := &Stream{s: s, id: id, service: service, canSend: window}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// Service is the name the opener asked for, e.g. "ledger".
func (st *Stream) Service() string { return st.service }

func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for len(st.buf) == 0 {
		switch {
		case st.err != nil:
			st.mu.Unlock()
			return 0, st.err
		case st.finIn:
			st.mu.Unlock()
			return 0, io.EOF
		case st.closed:
			st.mu.Unlock()
			return 0, ErrClosed
		}
		st.cond.Wait()
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	if len(st.buf) == 0 {
		st.buf = st.store[:0] // all read: start over at the front of the same memory
	}
	st.consumed += n
	var ack int
	if st.consumed >= window/2 { // acknowledge in batches, not per read
		ack, st.consumed = st.consumed, 0
	}
	st.mu.Unlock()

	if ack > 0 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(ack))
		st.s.writeFrame(frameWindow, st.id, b[:])
	}
	return n, nil
}

func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		for st.canSend == 0 && st.err == nil && !st.closed && !st.finOut {
			st.cond.Wait() // wait for the peer to acknowledge (WINDOW)
		}
		switch {
		case st.err != nil:
			st.mu.Unlock()
			return written, st.err
		case st.closed || st.finOut:
			st.mu.Unlock()
			return written, ErrClosed
		}
		k := min(len(p), st.canSend, maxPayload)
		st.canSend -= k
		st.mu.Unlock()

		if err := st.s.writeFrame(frameData, st.id, p[:k]); err != nil {
			return written, err
		}
		p = p[k:]
		written += k
	}
	return written, nil
}

// CloseWrite tells the peer we won't write more (it reads EOF).
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.finOut || st.closed || st.err != nil {
		st.mu.Unlock()
		return nil
	}
	st.finOut = true
	both := st.finIn
	st.cond.Broadcast()
	st.mu.Unlock()
	err := st.s.writeFrame(frameFin, st.id, nil)
	if both {
		st.s.forget(st.id)
	}
	return err
}

// Close ends the stream. If either side was still sending, the peer is told
// to abort (RST), like closing a TCP socket with unread data.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	clean := (st.finIn && st.finOut) || st.err != nil
	st.cond.Broadcast()
	st.mu.Unlock()
	st.s.forget(st.id)
	if !clean {
		st.s.writeFrame(frameRst, st.id, nil)
	}
	return nil
}

// push adds received data; false means the peer sent more than the window.
func (st *Stream) push(p []byte) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed || st.err != nil {
		return true // we're not reading anymore; drop it
	}
	if len(st.buf)+len(p) > window {
		return false
	}
	st.buf = append(st.buf, p...)
	if cap(st.buf) > cap(st.store) {
		st.store = st.buf[:0] // remember the (grown) memory for reuse
	}
	st.cond.Broadcast()
	return true
}

func (st *Stream) credit(n uint32) {
	st.mu.Lock()
	st.canSend += int(n)
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) remoteFin() {
	st.mu.Lock()
	st.finIn = true
	both := st.finOut
	st.cond.Broadcast()
	st.mu.Unlock()
	if both {
		st.s.forget(st.id)
	}
}

func (st *Stream) abort(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.cond.Broadcast()
	st.mu.Unlock()
	st.s.forget(st.id)
}
