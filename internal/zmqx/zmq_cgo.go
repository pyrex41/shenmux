//go:build cgo && (linux || darwin)

package zmqx

/*
#cgo linux LDFLAGS: -l:libzmq.so.5
#cgo darwin LDFLAGS: -lzmq
#include <errno.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// This deliberately declares the small stable libzmq C ABI surface we use
// instead of requiring development headers at runtime. zmq_msg_t has been a
// fixed 64-byte opaque object throughout the supported libzmq 4.x ABI.
typedef struct { unsigned char _[64]; } zmq_msg_t;

void *zmq_ctx_new(void);
int zmq_ctx_shutdown(void *context);
int zmq_ctx_term(void *context);
void *zmq_socket(void *context, int type);
int zmq_close(void *socket);
int zmq_bind(void *socket, const char *endpoint);
int zmq_connect(void *socket, const char *endpoint);
int zmq_setsockopt(void *socket, int option_name, const void *option_value, size_t option_len);
int zmq_getsockopt(void *socket, int option_name, void *option_value, size_t *option_len);
int zmq_msg_init(zmq_msg_t *msg);
int zmq_msg_init_size(zmq_msg_t *msg, size_t size);
int zmq_msg_close(zmq_msg_t *msg);
void *zmq_msg_data(zmq_msg_t *msg);
size_t zmq_msg_size(const zmq_msg_t *msg);
int zmq_msg_send(zmq_msg_t *msg, void *socket, int flags);
int zmq_msg_recv(zmq_msg_t *msg, void *socket, int flags);
int zmq_errno(void);
const char *zmq_strerror(int errnum);

static void shenmux_copy(void *dst, const void *src, size_t n) {
    if (n != 0) memcpy(dst, src, n);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Socket types from the stable ZeroMQ C API.
type SocketType int

const (
	Pair   SocketType = 0
	Pub    SocketType = 1
	Sub    SocketType = 2
	Dealer SocketType = 5
	Router SocketType = 6
	Pull   SocketType = 7
	Push   SocketType = 8
	XPub   SocketType = 9
)

// Send/receive flags.
const (
	DontWait = 1
	SndMore  = 2
)

// Socket options used by this project.
const (
	Identity    = 5
	Subscribe   = 6
	Unsubscribe = 7
	RcvMore     = 13
	Linger      = 17
	MaxMsgSize  = 22
	SndHWM      = 23
	RcvHWM      = 24
	RcvTimeout  = 27
	SndTimeout  = 28
	Immediate   = 39
	XPubVerbose = 40
)

const (
	defaultMaxFrame  = 256 << 20
	defaultMaxFrames = 16
)

var (
	ErrWouldBlock = errors.New("zmq operation would block")
	ErrClosed     = errors.New("zmq object is closed")
)

type Error struct {
	Operation string
	Code      int
	Message   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("zmq %s: %s (errno=%d)", e.Operation, e.Message, e.Code)
}

func lastError(operation string) error {
	return errorFromCode(operation, int(C.zmq_errno()))
}

func errorFromCode(operation string, code int) error {
	if code == int(C.EAGAIN) {
		return ErrWouldBlock
	}
	return &Error{Operation: operation, Code: code, Message: C.GoString(C.zmq_strerror(C.int(code)))}
}

type Context struct {
	mu     sync.Mutex
	ptr    unsafe.Pointer
	closed bool
}

func NewContext() (*Context, error) {
	ptr := C.zmq_ctx_new()
	if ptr == nil {
		return nil, lastError("ctx_new")
	}
	ctx := &Context{ptr: ptr}
	runtime.SetFinalizer(ctx, func(c *Context) { _ = c.Close() })
	return ctx, nil
}

func (c *Context) Socket(kind SocketType) (*Socket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ptr == nil {
		return nil, ErrClosed
	}
	ptr := C.zmq_socket(c.ptr, C.int(kind))
	if ptr == nil {
		return nil, lastError("socket")
	}
	s := &Socket{ptr: ptr, ctx: c}
	runtime.SetFinalizer(s, func(socket *Socket) { _ = socket.Close() })
	return s, nil
}

// Shutdown interrupts blocking calls on sockets created from this context.
func (c *Context) Shutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ptr == nil {
		return nil
	}
	if C.zmq_ctx_shutdown(c.ptr) != 0 {
		return lastError("ctx_shutdown")
	}
	return nil
}

func (c *Context) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ptr == nil {
		return nil
	}
	// ctx_term may block until all sockets close; callers should close sockets
	// first. A preceding shutdown ensures blocked socket operations wake up.
	_ = C.zmq_ctx_shutdown(c.ptr)
	if C.zmq_ctx_term(c.ptr) != 0 {
		return lastError("ctx_term")
	}
	c.ptr = nil
	c.closed = true
	runtime.SetFinalizer(c, nil)
	return nil
}

// Socket values must be owned by one goroutine at a time. ZeroMQ sockets are
// not thread-safe; this wrapper intentionally does not hide that constraint.
type Socket struct {
	mu     sync.Mutex // protects lifecycle only, not concurrent I/O
	ptr    unsafe.Pointer
	ctx    *Context
	closed bool
}

func (s *Socket) pointer() (unsafe.Pointer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ptr == nil {
		return nil, ErrClosed
	}
	return s.ptr, nil
}

func (s *Socket) Bind(endpoint string) error {
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	cEndpoint := C.CString(endpoint)
	defer C.free(unsafe.Pointer(cEndpoint))
	if C.zmq_bind(ptr, cEndpoint) != 0 {
		return lastError("bind")
	}
	return nil
}

func (s *Socket) Connect(endpoint string) error {
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	cEndpoint := C.CString(endpoint)
	defer C.free(unsafe.Pointer(cEndpoint))
	if C.zmq_connect(ptr, cEndpoint) != 0 {
		return lastError("connect")
	}
	return nil
}

func (s *Socket) SetInt(option, value int) error {
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	v := C.int(value)
	if C.zmq_setsockopt(ptr, C.int(option), unsafe.Pointer(&v), C.size_t(unsafe.Sizeof(v))) != 0 {
		return lastError("setsockopt")
	}
	return nil
}

// SetInt64 sets socket options whose stable ZeroMQ ABI type is int64_t, most
// notably ZMQ_MAXMSGSIZE. Using SetInt for these options silently passes the
// wrong option length on 64-bit platforms.
func (s *Socket) SetInt64(option int, value int64) error {
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	v := C.int64_t(value)
	if C.zmq_setsockopt(ptr, C.int(option), unsafe.Pointer(&v), C.size_t(unsafe.Sizeof(v))) != 0 {
		return lastError("setsockopt")
	}
	return nil
}

func (s *Socket) SetBytes(option int, value []byte) error {
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	var data unsafe.Pointer
	if len(value) != 0 {
		data = unsafe.Pointer(&value[0])
	}
	if C.zmq_setsockopt(ptr, C.int(option), data, C.size_t(len(value))) != 0 {
		return lastError("setsockopt")
	}
	runtime.KeepAlive(value)
	return nil
}

func (s *Socket) SendMultipart(frames [][]byte, flags int) error {
	if len(frames) == 0 {
		return errors.New("zmq multipart message needs at least one frame")
	}
	ptr, err := s.pointer()
	if err != nil {
		return err
	}
	for i, frame := range frames {
		var msg C.zmq_msg_t
		if C.zmq_msg_init_size(&msg, C.size_t(len(frame))) != 0 {
			return lastError("msg_init_size")
		}
		if len(frame) != 0 {
			C.shenmux_copy(C.zmq_msg_data(&msg), unsafe.Pointer(&frame[0]), C.size_t(len(frame)))
		}
		sendFlags := flags
		if i+1 < len(frames) {
			sendFlags |= SndMore
		}
		if C.zmq_msg_send(&msg, ptr, C.int(sendFlags)) < 0 {
			_ = C.zmq_msg_close(&msg)
			return lastError("msg_send")
		}
		if C.zmq_msg_close(&msg) != 0 {
			return lastError("msg_close")
		}
		runtime.KeepAlive(frame)
	}
	return nil
}

func (s *Socket) RecvMultipart(flags int) ([][]byte, error) {
	return s.RecvMultipartLimit(flags, defaultMaxFrame, defaultMaxFrames)
}

func (s *Socket) RecvMultipartLimit(flags, maxFrame, maxFrames int) ([][]byte, error) {
	if maxFrame <= 0 || maxFrames <= 0 {
		return nil, errors.New("zmq receive limits must be positive")
	}
	ptr, err := s.pointer()
	if err != nil {
		return nil, err
	}
	frames := make([][]byte, 0, 3)
	for {
		if len(frames) >= maxFrames {
			return nil, fmt.Errorf("zmq multipart message exceeds %d frames", maxFrames)
		}
		var msg C.zmq_msg_t
		if C.zmq_msg_init(&msg) != 0 {
			return nil, lastError("msg_init")
		}
		recvFlags := flags
		if len(frames) != 0 {
			// Once the first frame is available, the remainder of that atomic
			// multipart message is immediately available.
			recvFlags &^= DontWait
		}
		if C.zmq_msg_recv(&msg, ptr, C.int(recvFlags)) < 0 {
			// Capture errno before closing the message; libzmq may update
			// errno during zmq_msg_close on interrupted receives.
			code := int(C.zmq_errno())
			_ = C.zmq_msg_close(&msg)
			// Signals such as SIGCHLD can interrupt libzmq even when the
			// receive is non-blocking. Retry the same frame; callers should
			// only observe transport errors that cannot be recovered this way.
			if code == int(C.EINTR) {
				continue
			}
			// Some Darwin/libzmq combinations report ETIMEDOUT for a
			// nonblocking receive with no message. DONTWAIT makes this the
			// same recoverable condition as EAGAIN.
			if recvFlags&DontWait != 0 && code == int(C.ETIMEDOUT) {
				return nil, ErrWouldBlock
			}
			return nil, errorFromCode("msg_recv", code)
		}
		size := int(C.zmq_msg_size(&msg))
		if size < 0 || size > maxFrame {
			_ = C.zmq_msg_close(&msg)
			return nil, fmt.Errorf("zmq frame size %d exceeds limit %d", size, maxFrame)
		}
		var frame []byte
		if size != 0 {
			frame = C.GoBytes(C.zmq_msg_data(&msg), C.int(size))
		} else {
			frame = []byte{}
		}
		if C.zmq_msg_close(&msg) != 0 {
			return nil, lastError("msg_close")
		}
		frames = append(frames, frame)

		var more C.int
		length := C.size_t(unsafe.Sizeof(more))
		if C.zmq_getsockopt(ptr, C.int(RcvMore), unsafe.Pointer(&more), &length) != 0 {
			return nil, lastError("getsockopt")
		}
		if more == 0 {
			return frames, nil
		}
	}
}

func (s *Socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ptr == nil {
		return nil
	}
	if C.zmq_close(s.ptr) != 0 {
		return lastError("close")
	}
	s.ptr = nil
	s.closed = true
	runtime.SetFinalizer(s, nil)
	return nil
}
