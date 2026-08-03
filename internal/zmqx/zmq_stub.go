//go:build !cgo || (!linux && !darwin)

package zmqx

import "errors"

type SocketType int

const (
	Pair SocketType = iota
	Pub
	Sub
	_ // req
	_ // rep
	Dealer
	Router
	Pull
	Push
	XPub
)

const (
	DontWait = 1
	SndMore  = 2
)

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

var (
	ErrWouldBlock  = errors.New("zmq operation would block")
	ErrClosed      = errors.New("zmq object is closed")
	ErrUnsupported = errors.New("zmq requires cgo and libzmq on Linux or macOS")
)

type Context struct{}
type Socket struct{}

func NewContext() (*Context, error)                                { return nil, ErrUnsupported }
func (*Context) Socket(SocketType) (*Socket, error)                { return nil, ErrUnsupported }
func (*Context) Shutdown() error                                   { return nil }
func (*Context) Close() error                                      { return nil }
func (*Socket) Bind(string) error                                  { return ErrUnsupported }
func (*Socket) Connect(string) error                               { return ErrUnsupported }
func (*Socket) SetInt(int, int) error                              { return ErrUnsupported }
func (*Socket) SetInt64(int, int64) error                          { return ErrUnsupported }
func (*Socket) SetBytes(int, []byte) error                         { return ErrUnsupported }
func (*Socket) SendMultipart([][]byte, int) error                  { return ErrUnsupported }
func (*Socket) RecvMultipart(int) ([][]byte, error)                { return nil, ErrUnsupported }
func (*Socket) RecvMultipartLimit(int, int, int) ([][]byte, error) { return nil, ErrUnsupported }
func (*Socket) Close() error                                       { return nil }
