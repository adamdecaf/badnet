package badnet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTargetAddress(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"127.0.0.1:9119", "127.0.0.1:9119"},
		{"http://127.0.0.1:9119", "127.0.0.1:9119"},
		{"https://127.0.0.1:9119", "127.0.0.1:9119"},
		{"https://example.com", "example.com:443"},
		{"HTTPS://example.com", "example.com:443"},
		{"wss://example.com", "example.com:443"},
		{"http://example.com", "example.com:80"},
		{"ws://example.com", "example.com:80"},
		{"example.com", "example.com:80"},
		{"example.com:81", "example.com:81"},
		{"example.com:", "example.com:80"},
		{"[::1]:8080", "[::1]:8080"},
		{"[::1]", "[::1]:80"},
		{"::1", "[::1]:80"},
		{"http://[::1]", "[::1]:80"},
		{"http://[::1]:9000", "[::1]:9000"},
		{"https://[::1]", "[::1]:443"},
		{"https://example.com:8443", "example.com:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, Config{Target: tc.in}.targetAddress())
		})
	}
}

func TestJoinHostPortDefault(t *testing.T) {
	require.Equal(t, "h:80", joinHostPortDefault("h", "80"))
	require.Equal(t, "h:9", joinHostPortDefault("h:9", "80"))
	require.Equal(t, "h:80", joinHostPortDefault("h:", "80"))
}

func TestDialTimeout(t *testing.T) {
	require.Equal(t, defaultDialTimeout, Config{}.dialTimeout())
	require.Equal(t, 2*time.Second, Config{DialTimeout: 2 * time.Second}.dialTimeout())
}

func TestDirectionActive(t *testing.T) {
	require.False(t, Direction{}.active())
	require.True(t, Direction{MaxKBps: 1}.active())
	require.True(t, Direction{Latency: time.Millisecond}.active())
	require.True(t, Direction{FailureRatio: 1}.active())
}

func TestShouldFail(t *testing.T) {
	require.False(t, shouldFail(0))
	require.False(t, shouldFail(-1))
	require.True(t, shouldFail(100))
	require.True(t, shouldFail(150))

	var hits int
	for i := 0; i < 400; i++ {
		if shouldFail(50) {
			hits++
		}
	}
	require.InDelta(t, 200, hits, 80)
}

func TestPartialLen(t *testing.T) {
	require.Equal(t, 0, partialLen(0))
	require.Equal(t, 1, partialLen(1))
	require.Equal(t, 1, partialLen(2))
	require.Equal(t, 5, partialLen(10))
}

func TestSleepAndThrottleNoop(t *testing.T) {
	sleep(0)
	throttle(0, 10)
	throttle(10, 0)
	throttle(-1, 1)
}

func TestIsBenign(t *testing.T) {
	require.True(t, isBenign(nil))
	require.True(t, isBenign(io.EOF))
	require.True(t, isBenign(net.ErrClosed))
	require.True(t, isBenign(io.ErrClosedPipe))
	require.True(t, isBenign(errors.New("read: use of closed network connection")))
	require.True(t, isBenign(&timeoutErr{}))
	require.False(t, isBenign(io.ErrUnexpectedEOF))
	require.False(t, isBenign(io.ErrShortWrite))
}

func TestRetryableAccept(t *testing.T) {
	require.False(t, retryableAccept(nil))
	require.False(t, retryableAccept(net.ErrClosed))
	require.False(t, retryableAccept(errors.New("permanent")))
	require.True(t, retryableAccept(&timeoutErr{}))
	require.True(t, retryableAccept(&tempErr{}))
}

func TestPortAndBindAddr(t *testing.T) {
	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: "127.0.0.1:9",
	})
	require.NotEmpty(t, proxy.BindAddr())
	require.Greater(t, proxy.Port(), 0)
	require.LessOrEqual(t, proxy.Port(), 65535)

	require.Equal(t, -1, (&Proxy{bindAddr: "not-an-addr"}).Port())
	require.Equal(t, -1, (&Proxy{bindAddr: "127.0.0.1:abc"}).Port())
}

func TestFailureRatioEmpty(t *testing.T) {
	require.Equal(t, 0.0, (&Proxy{}).FailureRatio())
}

func TestNewProxyListenError(t *testing.T) {
	_, err := newProxy(Config{Listen: "256.256.256.256:1"})
	require.Error(t, err)
}

func TestTransparentEcho(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{Listen: "127.0.0.1:0", Target: addr})

	conn, err := net.Dial("tcp", proxy.BindAddr())
	require.NoError(t, err)
	defer conn.Close()

	msg := []byte("hello-transparent-proxy")
	_, err = conn.Write(msg)
	require.NoError(t, err)

	got := make([]byte, len(msg))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, msg, got)
	require.Equal(t, 0.0, proxy.FailureRatio())
}

func TestHalfCloseResponse(t *testing.T) {
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

	// Wrapped (latency) and raw paths both need to forward FIN.
	for _, name := range []string{"raw", "wrapped"} {
		t.Run(name, func(t *testing.T) {
			conf := Config{Listen: "127.0.0.1:0", Target: ln.Addr().String()}
			if name == "wrapped" {
				conf.Read.Latency = time.Millisecond
			}
			proxy := ForTest(t, conf)

			conn, err := net.Dial("tcp", proxy.BindAddr())
			require.NoError(t, err)
			defer conn.Close()

			_, err = conn.Write([]byte("ping"))
			require.NoError(t, err)
			require.NoError(t, conn.(*net.TCPConn).CloseWrite())

			got, err := io.ReadAll(conn)
			require.NoError(t, err)
			require.Equal(t, "got:ping", string(got))
		})
	}
}

func TestReadFailureDoesNotAffectWrites(t *testing.T) {
	var got atomic.Value
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, _ := io.ReadFull(c, buf[:4])
		got.Store(string(buf[:n]))
		_, _ = c.Write([]byte("RESP"))
	}()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: ln.Addr().String(),
		Read:   Direction{FailureRatio: 100},
	})

	conn, err := net.DialTimeout("tcp", proxy.BindAddr(), time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	_, err = conn.Write([]byte("PING"))
	require.NoError(t, err)

	body, _ := io.ReadAll(conn)
	require.NotEqual(t, "RESP", string(body))
	require.Eventually(t, func() bool {
		v, _ := got.Load().(string)
		return v == "PING"
	}, 2*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return proxy.FailureRatio() > 0 }, 2*time.Second, 10*time.Millisecond)
}

func TestWriteFailureTruncatesRequest(t *testing.T) {
	var nGot atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		n, _ := c.Read(buf)
		nGot.Store(int32(n))
	}()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: ln.Addr().String(),
		Write:  Direction{FailureRatio: 100},
	})

	conn, err := net.DialTimeout("tcp", proxy.BindAddr(), time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	payload := make([]byte, 64)
	for i := range payload {
		payload[i] = 'A'
	}
	_, _ = conn.Write(payload)
	buf := make([]byte, 8)
	_, _ = conn.Read(buf)

	require.Eventually(t, func() bool { return nGot.Load() > 0 }, 2*time.Second, 10*time.Millisecond)
	require.Less(t, nGot.Load(), int32(len(payload)))
	require.Greater(t, proxy.FailureRatio(), 0.0)
}

func TestReadLatency(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: addr,
		Read:   Direction{Latency: 80 * time.Millisecond},
	})

	conn, err := net.Dial("tcp", proxy.BindAddr())
	require.NoError(t, err)
	defer conn.Close()

	start := time.Now()
	_, err = conn.Write([]byte("abcd"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 80*time.Millisecond)
	require.Less(t, elapsed, 400*time.Millisecond)
}

func TestWriteLatency(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: addr,
		Write:  Direction{Latency: 80 * time.Millisecond},
	})

	conn, err := net.Dial("tcp", proxy.BindAddr())
	require.NoError(t, err)
	defer conn.Close()

	start := time.Now()
	_, err = conn.Write([]byte("abcd"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 80*time.Millisecond)
}

func TestBandwidthLimit(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: addr,
		Read:   Direction{MaxKBps: 4},
	})

	conn, err := net.Dial("tcp", proxy.BindAddr())
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	payload := make([]byte, 2048)
	start := time.Now()
	_, err = conn.Write(payload)
	require.NoError(t, err)
	_, err = io.ReadFull(conn, make([]byte, len(payload)))
	require.NoError(t, err)
	// 2048 bytes at 4 KBps ≈ 500ms
	require.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond)
}

func TestDialFailureDoesNotFailTest(t *testing.T) {
	proxy := ForTest(t, Config{
		Listen:      "127.0.0.1:0",
		Target:      "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
	})

	conn, err := net.DialTimeout("tcp", proxy.BindAddr(), time.Second)
	require.NoError(t, err)
	_, _ = conn.Write([]byte("x"))
	buf := make([]byte, 8)
	_, _ = conn.Read(buf)
	conn.Close()

	require.Eventually(t, func() bool {
		return proxy.connectionCount.Load() >= 1 && proxy.targetFailures.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 1.0, proxy.FailureRatio())
}

func TestHTTPStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PONG"))
	}))
	t.Cleanup(server.Close)

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: server.Listener.Addr().String(),
		Read:   Direction{FailureRatio: 25},
		Write:  Direction{FailureRatio: 25},
	})

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	address := "http://" + proxy.BindAddr()
	for i := 0; i < 80; i++ {
		resp, err := client.Get(address)
		if err == nil && resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	require.InDelta(t, 0.44, proxy.FailureRatio(), 0.3)
}

func TestHTTPKeepAliveTransparent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: server.Listener.Addr().String(),
	})

	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 3; i++ {
		resp, err := client.Get("http://" + proxy.BindAddr())
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, "ok", string(body))
	}
}

func TestWrapConnPassthrough(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	require.Equal(t, a, wrapConn(a, Config{}))
	wrapped := wrapConn(a, Config{Read: Direction{Latency: time.Millisecond}})
	_, ok := wrapped.(*conn)
	require.True(t, ok)
	_ = b
}

func TestConnPartialIO(t *testing.T) {
	t.Run("write short", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		w := wrapConn(client, Config{Read: Direction{FailureRatio: 100}})
		var wg sync.WaitGroup
		wg.Add(1)
		var got []byte
		go func() {
			defer wg.Done()
			buf := make([]byte, 64)
			n, _ := server.Read(buf)
			got = append([]byte(nil), buf[:n]...)
		}()

		n, err := w.Write([]byte("abcdefgh"))
		require.ErrorIs(t, err, io.ErrShortWrite)
		require.Equal(t, 4, n)
		wg.Wait()
		require.Equal(t, "abcd", string(got))
	})

	t.Run("read unexpected EOF", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		w := wrapConn(client, Config{Write: Direction{FailureRatio: 100}})
		go func() { _, _ = server.Write([]byte("abcdefgh")) }()

		buf := make([]byte, 8)
		n, err := w.Read(buf)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Equal(t, 4, n)
	})

	t.Run("read one byte", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		w := wrapConn(client, Config{Write: Direction{FailureRatio: 100}})
		go func() { _, _ = server.Write([]byte("Z")) }()
		buf := make([]byte, 1)
		n, err := w.Read(buf)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Equal(t, 1, n)
	})

	t.Run("empty write fail", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		w := wrapConn(client, Config{Read: Direction{FailureRatio: 100}})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.Copy(io.Discard, server)
		}()
		n, err := w.Write(nil)
		require.ErrorIs(t, err, io.ErrShortWrite)
		require.Equal(t, 0, n)
		client.Close()
		<-done
	})
}

func TestConnUnderlyingErrors(t *testing.T) {
	t.Run("write to closed", func(t *testing.T) {
		client, server := net.Pipe()
		server.Close()
		client.Close()
		w := wrapConn(client, Config{Read: Direction{FailureRatio: 100}})
		_, err := w.Write([]byte("hi"))
		require.Error(t, err)
	})

	t.Run("read from closed", func(t *testing.T) {
		client, server := net.Pipe()
		server.Close()
		client.Close()
		w := wrapConn(client, Config{Write: Direction{FailureRatio: 100}})
		_, err := w.Read(make([]byte, 8))
		require.Error(t, err)
	})

	t.Run("normal read write with rate", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		w := wrapConn(client, Config{
			Read:  Direction{MaxKBps: 100},
			Write: Direction{MaxKBps: 100},
		})
		go func() {
			buf := make([]byte, 2048)
			_, _ = server.Read(buf)
			_, _ = server.Write([]byte("xy"))
		}()
		_, err := w.Write([]byte("ok"))
		require.NoError(t, err)
		buf := make([]byte, 2048)
		n, err := w.Read(buf)
		require.NoError(t, err)
		require.Equal(t, "xy", string(buf[:n]))
	})
}

func TestCloseWrite(t *testing.T) {
	t.Run("forwards", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()
		var accepted net.Conn
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted = c
			_, _ = io.ReadAll(c)
		}()
		raw, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		defer raw.Close()
		w := wrapConn(raw, Config{Read: Direction{Latency: time.Millisecond}})
		require.NoError(t, w.(closeWriter).CloseWrite())
		<-done
		if accepted != nil {
			accepted.Close()
		}
	})

	t.Run("noop without CloseWrite", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		w := wrapConn(a, Config{Write: Direction{Latency: time.Millisecond}})
		require.NoError(t, w.(closeWriter).CloseWrite())
		closeWrite(a) // raw pipe: no CloseWrite
	})
}

func TestCopyDir(t *testing.T) {
	t.Run("counts non-benign", func(t *testing.T) {
		src, wr := net.Pipe()
		defer src.Close()
		defer wr.Close()
		dst := writeFuncConn{
			Conn: src,
			fn:   func([]byte) (int, error) { return 0, io.ErrShortWrite },
		}

		var counter atomic.Uint32
		errc := make(chan error, 1)
		go func() { errc <- copyDir(dst, src, &counter) }()
		_, _ = wr.Write([]byte("hello"))
		err := <-errc
		require.Error(t, err)
		require.Equal(t, uint32(1), counter.Load())
	})

	t.Run("benign closed", func(t *testing.T) {
		src, wr := net.Pipe()
		dst, rd := net.Pipe()
		defer rd.Close()
		wr.Close()
		src.Close()
		var counter atomic.Uint32
		go func() {
			buf := make([]byte, 4)
			_, _ = rd.Read(buf)
		}()
		_ = copyDir(dst, src, &counter)
		dst.Close()
		require.Equal(t, uint32(0), counter.Load())
	})
}

func TestServeAcceptErrors(t *testing.T) {
	t.Run("retry then close", func(t *testing.T) {
		ln := &scriptedListener{results: []acceptResult{
			{err: &timeoutErr{}},
			{err: net.ErrClosed},
		}}
		p := &Proxy{listenerClosed: make(chan struct{})}
		p.serve(ln)
		_, ok := <-p.listenerClosed
		require.False(t, ok)
		require.GreaterOrEqual(t, ln.calls.Load(), int32(2))
	})

	t.Run("permanent stops", func(t *testing.T) {
		ln := &scriptedListener{results: []acceptResult{
			{err: errors.New("boom")},
		}}
		p := &Proxy{listenerClosed: make(chan struct{})}
		p.serve(ln)
		_, ok := <-p.listenerClosed
		require.False(t, ok)
	})

	t.Run("temp then closed", func(t *testing.T) {
		ln := &scriptedListener{results: []acceptResult{
			{err: &tempErr{}},
			{err: net.ErrClosed},
		}}
		p := &Proxy{listenerClosed: make(chan struct{})}
		p.serve(ln)
		<-p.listenerClosed
	})
}

func TestShutdownNilListener(t *testing.T) {
	p := &Proxy{listenerClosed: make(chan struct{})}
	close(p.listenerClosed)
	p.shutdown()
}

func TestBothDirectionsFail(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: addr,
		Read:   Direction{FailureRatio: 100},
		Write:  Direction{FailureRatio: 100},
	})

	conn, err := net.Dial("tcp", proxy.BindAddr())
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte("hello-world"))
	_, _ = io.ReadAll(conn)

	require.Eventually(t, func() bool { return proxy.FailureRatio() == 1 }, 2*time.Second, 10*time.Millisecond)
}

func TestHTTPSTargetDials443Scheme(t *testing.T) {
	require.Equal(t, "example.com:443", Config{Target: "https://example.com"}.targetAddress())
}

func TestHandleWhileClosing(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	p := &Proxy{listenerClosed: make(chan struct{})}
	p.closing.Store(true)
	p.wg.Add(1)
	p.handle(a)
	require.Equal(t, uint32(1), p.connectionCount.Load())
	require.Equal(t, uint32(0), p.targetFailures.Load())
}

type writeFuncConn struct {
	net.Conn
	fn func([]byte) (int, error)
}

func (c writeFuncConn) Write(p []byte) (int, error) { return c.fn(p) }

func TestWriteChunkErrors(t *testing.T) {
	raw, peer := net.Pipe()
	t.Cleanup(func() {
		raw.Close()
		peer.Close()
	})

	t.Run("underlying error", func(t *testing.T) {
		w := wrapConn(writeFuncConn{
			Conn: raw,
			fn:   func([]byte) (int, error) { return 0, io.ErrClosedPipe },
		}, Config{Read: Direction{MaxKBps: 200}})
		_, err := w.Write(make([]byte, 1500))
		require.Error(t, err)
	})

	t.Run("short write", func(t *testing.T) {
		w := wrapConn(writeFuncConn{
			Conn: raw,
			fn:   func([]byte) (int, error) { return 1, nil },
		}, Config{Read: Direction{MaxKBps: 200}})
		_, err := w.Write(make([]byte, 1500))
		require.ErrorIs(t, err, io.ErrShortWrite)
	})
}

func TestWriteRateLimitChunks(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	w := wrapConn(client, Config{Read: Direction{MaxKBps: 8}})
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(server)
		done <- buf
	}()
	payload := make([]byte, 2048)
	start := time.Now()
	n, err := w.Write(payload)
	require.NoError(t, err)
	require.Equal(t, 2048, n)
	require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	client.Close()
	require.Len(t, <-done, 2048)
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

type tempErr struct{}

func (tempErr) Error() string   { return "temp" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

type acceptResult struct {
	c   net.Conn
	err error
}

type scriptedListener struct {
	results []acceptResult
	mu      sync.Mutex
	calls   atomic.Int32
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) == 0 {
		return nil, net.ErrClosed
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r.c, r.err
}

func (s *scriptedListener) Close() error { return nil }

func (s *scriptedListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:0" }

func startEcho(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln, ln.Addr().String()
}

func TestConcurrentEcho(t *testing.T) {
	srv, addr := startEcho(t)
	defer srv.Close()

	proxy := ForTest(t, Config{
		Listen: "127.0.0.1:0",
		Target: addr,
		Read:   Direction{FailureRatio: 20, Latency: 10 * time.Millisecond},
		Write:  Direction{FailureRatio: 20, Latency: 10 * time.Millisecond},
	})

	const n = 10
	const size = 1024
	var ok, bad atomic.Int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", proxy.BindAddr(), 2*time.Second)
			if err != nil {
				bad.Add(1)
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			for i := 0; i < 5; i++ {
				msg := bytes.Repeat([]byte("x"), size)
				if _, err := c.Write(msg); err != nil {
					bad.Add(1)
					return
				}
				got := make([]byte, size)
				if _, err := io.ReadFull(c, got); err != nil {
					bad.Add(1)
					return
				}
				if bytes.Equal(got, msg) {
					ok.Add(1)
				} else {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	require.Greater(t, ok.Load()+bad.Load(), int32(0))
	require.Greater(t, ok.Load(), int32(0))
}

func TestHTTPServerShutdown(t *testing.T) {
	// keep context import used if other tests need it; exercise proxy cleanup
	// against a real http.Server with a short timeout.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	})
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go hs.Serve(ln)
	t.Cleanup(func() { _ = hs.Shutdown(context.Background()) })

	proxy := ForTest(t, Config{Listen: "127.0.0.1:0", Target: ln.Addr().String()})
	resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + proxy.BindAddr())
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "x", string(body))
}
