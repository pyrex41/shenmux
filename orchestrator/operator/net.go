package operator

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// PickFreePort asks the kernel for a free loopback TCP port by binding
// 127.0.0.1:0, reading the assigned port, and releasing it. There is an
// inherent race (the port could be taken before the caller re-binds), but for a
// local single-machine demo it is reliable enough and avoids blind guessing.
func PickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("listener address is not TCP")
	}
	return addr.Port, nil
}

// waitForPort polls until 127.0.0.1:port accepts a connection or the deadline
// passes. If alive is non-nil it is consulted between polls; when it reports the
// backing process has exited, waiting stops early with an error.
func waitForPort(port int, timeout time.Duration, alive func() bool) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for {
		if alive != nil && !alive() {
			return fmt.Errorf("process for %s exited before it began listening", addr)
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to accept connections: %w", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
