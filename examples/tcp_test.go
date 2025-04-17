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

func TestConcurrentTCPConnections(t *testing.T) {
	// Start a simple TCP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverAddr := listener.Addr().String()
	defer listener.Close()

	// Server echos back received data
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

	// Create proxy with moderate failure ratio
	proxy := badnet.ForTest(t, badnet.Config{
		Listen: "127.0.0.1:0",
		Target: serverAddr,
		Read: badnet.Direction{
			FailureRatio: 5,
			Latency:      10 * time.Millisecond,
		},
		Write: badnet.Direction{
			FailureRatio: 5,
			Latency:      10 * time.Millisecond,
		},
	})

	// Number of concurrent connections
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

	// Start concurrent clients
	wg.Add(numConns)
	for i := 0; i < numConns; i++ {
		go func(clientID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", proxy.BindAddr())
			if err != nil {
				t.Logf("ERROR setting up connection through %s proxy: %v", proxy.BindAddr(), err)
				failed.Add(1)
				return
			}
			defer conn.Close()

			// Send and receive multiple messages
			for j := 0; j < messagesPerConn; j++ {
				// Unique message for this client and iteration
				message := fmt.Sprintf("client-%d-msg-%d-%s", clientID, j, randomString(msgSize-20))
				_, err = conn.Write([]byte(message))
				if err != nil {
					failed.Add(1)
					continue
				}

				// Read response
				buf := make([]byte, msgSize)
				n, err := conn.Read(buf)
				if err != nil {
					if err == io.EOF || err == io.ErrUnexpectedEOF {
						partial.Add(1)
					} else {
						failed.Add(1)
					}
					continue
				}

				received := string(buf[:n])
				if received == message {
					successful.Add(1)
				} else {
					partial.Add(1)
				}
			}
		}(i)
	}

	// Wait for all clients to finish
	wg.Wait()

	// Verify results
	require.Greater(t, numConns, 0)
	require.Greater(t, int(successful.Load()+partial.Load()+failed.Load()), 0)

	// Expect some successes and some failures
	totalAttempts := numConns * messagesPerConn
	require.GreaterOrEqual(t, successful.Load(), int32(totalAttempts/3))
	require.GreaterOrEqual(t, partial.Load()+failed.Load(), int32(totalAttempts/10), "should have some failures due to 20% failure ratio")

	// Verify proxy stats
	failureRatio := proxy.FailureRatio()
	require.Greater(t, failureRatio, 0.0)
	require.Less(t, failureRatio, 0.75)

	// Verify server saw connections
	require.GreaterOrEqual(t, serverConnections.Load(), uint32(numConns), "server should have handled at least numConns connections")

	// Verify latency
	elapsed := time.Since(clientStartTime)
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "total duration should account for latency")
}

// Helper to generate a random string
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[i%len(letters)]
	}
	return string(b)
}

// Helper to check for connection closed errors
func isClosedError(err error) bool {
	if err != nil {
		if err == net.ErrClosed {
			return true
		}
		return strings.Contains(err.Error(), "use of closed network connection")
	}
	return false
}
