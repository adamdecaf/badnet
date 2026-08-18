package tests

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adamdecaf/badnet"

	"github.com/stretchr/testify/require"
)

// Concurrent echo through a proxy with latency and per-connection failures.
func TestConcurrentTCPConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverAddr := listener.Addr().String()
	defer listener.Close()

	var serverConnections atomic.Uint32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !isClosedError(err) {
					t.Error("server accept error:", err)
				}
				return
			}
			serverConnections.Add(1)
			go func(conn net.Conn) {
				defer conn.Close()
				_, err := io.Copy(conn, conn)
				if err != nil && !isClosedError(err) {
					t.Error("server copy error:", err)
				}
			}(conn)
		}
	}()

	proxy := badnet.ForTest(t, badnet.Config{
		Listen: "127.0.0.1:0",
		Target: serverAddr,
		Read: badnet.Direction{
			FailureRatio: 20,
			Latency:      10 * time.Millisecond,
		},
		Write: badnet.Direction{
			FailureRatio: 20,
			Latency:      10 * time.Millisecond,
		},
	})

	const numConns = 10
	const msgSize = 1024
	const messagesPerConn = 5

	var (
		wg              sync.WaitGroup
		successful      atomic.Int32
		partial         atomic.Int32
		failed          atomic.Int32
		clientStartTime = time.Now()
	)

	wg.Add(numConns)
	for i := 0; i < numConns; i++ {
		go func(clientID int) {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", proxy.BindAddr(), 2*time.Second)
			if err != nil {
				t.Logf("ERROR setting up connection through %s proxy: %v", proxy.BindAddr(), err)
				failed.Add(1)
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

			for j := 0; j < messagesPerConn; j++ {
				msg := []byte(fmt.Sprintf("client-%d-msg-%d-%s", clientID, j, randomString(msgSize-32)))
				_, err = conn.Write(msg)
				if err != nil {
					failed.Add(1)
					return
				}

				buf := make([]byte, len(msg))
				_, err := io.ReadFull(conn, buf)
				if err != nil {
					if err == io.EOF || err == io.ErrUnexpectedEOF || err == io.ErrShortWrite {
						partial.Add(1)
					} else {
						failed.Add(1)
					}
					return
				}

				if string(buf) == string(msg) {
					successful.Add(1)
				} else {
					partial.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()

	require.Greater(t, int(successful.Load()+partial.Load()+failed.Load()), 0)
	require.Greater(t, successful.Load(), int32(0))
	require.GreaterOrEqual(t, serverConnections.Load(), uint32(1))
	require.GreaterOrEqual(t, proxy.FailureRatio(), 0.0)
	require.LessOrEqual(t, proxy.FailureRatio(), 1.0)

	elapsed := time.Since(clientStartTime)
	require.GreaterOrEqual(t, elapsed, 10*time.Millisecond)
}

// A client CloseWrite is forwarded so the server can reply after EOF.
func TestHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				body, err := io.ReadAll(c)
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("got:" + string(body)))
			}(c)
		}
	}()

	proxy := badnet.ForTest(t, badnet.Config{
		Listen: "127.0.0.1:0",
		Target: ln.Addr().String(),
		Read:   badnet.Direction{Latency: time.Millisecond},
	})

	conn, err := net.DialTimeout("tcp", proxy.BindAddr(), 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())

	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "got:ping", string(got))
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[i%len(letters)]
	}
	return string(b)
}

func isClosedError(err error) bool {
	if err != nil {
		if err == net.ErrClosed {
			return true
		}
		return strings.Contains(err.Error(), "use of closed network connection")
	}
	return false
}
