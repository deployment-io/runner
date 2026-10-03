package client

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"testing"
	"time"
)

// A save to a server that accepts the connection but never answers returns the deadline error once
// its timeout passes, and leaves the shared connection alone.
func TestSaveInfraContext_StalledServerTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn // hold it open, never read or reply
		}
	}()
	defer func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	}()

	shared, sharedServer := net.Pipe()
	defer sharedServer.Close()
	r := &RunnerClient{
		c:           rpc.NewClient(shared),
		isConnected: true,
		dial:        dialerFor(Options{Service: listener.Addr().String()}),
	}
	defer r.c.Close()

	start := time.Now()
	_, err = r.SaveInfraContext("org", "[]", 100*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SaveInfraContext took %s against a stalled server, want ~100ms", elapsed)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}
