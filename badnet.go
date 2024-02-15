package badnet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go4.org/net/throttle"
)

type Config struct {
	Listen, Target string

	Read  Direction
	Write Direction
}

func (c Config) targetAddress() string {
	host := c.Target

	u, _ := url.Parse(host)
	if u != nil && u.Host != "" {
		host = u.Host
	} else {
		host = c.Target
	}

	host, port, _ := net.SplitHostPort(host)
	if host == "" {
		if u != nil && u.Host != "" {
			host = u.Host
		} else {
			host = c.Target
		}
	}
	if port == "" {
		port = "80"
	}
	return host + ":" + port
}

type Direction struct {
	MaxKBps      int // set 0 for unlimited
	Latency      time.Duration
	FailureRatio int
}

type Proxy struct {
	conf Config

	bindAddr string

	// various statistics
	connectionCount atomic.Uint32
	readFailures    atomic.Uint32
	writeFailures   atomic.Uint32
	targetFailures  atomic.Uint32
}

func ForTest(t *testing.T, conf Config) *Proxy {
	t.Helper()

	p := &Proxy{
		conf: conf,
	}
	var err error

	// Setup listener
	ln, err := newListener(p.conf)
	if err != nil {
		t.Fatalf("badnet listen failed: %v", err)
	}
	p.bindAddr = ln.Addr().String()

	// Cycle through connections to proxy traffic
	ctx, cancelFunc := context.WithCancel(context.Background())

	t.Cleanup(func() { ln.Close() })
	t.Cleanup(func() { cancelFunc() })

	go func(ctx context.Context, ln net.Listener) { //nolint:staticcheck
		for {
			// Block while waiting for a connection
			connCh := make(chan net.Conn)
			go func() { //nolint:staticcheck
				conn, err := ln.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						t.Fatalf("badnet listener accept error: %v", err) //nolint:govet,staticcheck
					}
					return
				}
				p.connectionCount.Add(1)
				connCh <- conn
			}()

			select {
			case <-ctx.Done():
				close(connCh)
				return

			case conn := <-connCh:
				// Connect to the target
				target, err := net.Dial("tcp", p.conf.targetAddress())
				if err != nil {
					p.targetFailures.Add(1)
					t.Fatalf("connecting to %s failed: %v", p.conf.targetAddress(), err) //nolint:govet,staticcheck
					return
				}

				// pipe between the listener and target in both directions
				errCh := make(chan error, 1)
				go pipe(errCh, conn, target, &p.readFailures)
				go pipe(errCh, target, conn, &p.writeFailures)
				<-errCh

				// Cleanup after ourselves
				target.Close()
				conn.Close()
				close(connCh)
			}
		}
	}(ctx, ln)

	return p
}

func (p *Proxy) BindAddr() string {
	return p.bindAddr
}

func (p *Proxy) Port() int {
	_, port, err := net.SplitHostPort(p.BindAddr())
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return -1
	}
	return int(n)
}

// FailureRatio is a ratio of the injected failures and failures to connect with the target
// against the overall number of connections made to the proxy.
func (p *Proxy) FailureRatio() float64 {
	connections := float64(p.connectionCount.Load())
	failures := float64(p.readFailures.Load() + p.writeFailures.Load() + p.targetFailures.Load())
	return failures / connections
}

type conn struct {
	net.Conn

	targetAddress string

	readFailureRatio  int // 1-100%
	writeFailureRatio int // 1-100%
}

var (
	maxChoice = big.NewInt(int64(100))
)

func shouldFail(ratio int) bool {
	n, _ := rand.Int(rand.Reader, maxChoice)
	return n.Int64() <= int64(ratio)
}

func (c *conn) Read(b []byte) (n int, err error) {
	if c.targetAddress != "" {
		// Our target is accessed with a hostname, so if the request looks like HTTP
		// we need to make sure that the 'Host' header has the hostname.
		//
		// If we send the request with an IP the server won't understand our request.
		//
		// TODO(adam): Implement a more generic replacement procedure.

		// Read the HTTP request and replace the header
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b)))
		if err != nil {
			goto read
		}

		var beforeBuf bytes.Buffer
		req.Write(&beforeBuf)

		// Replace the Host header with our target
		host, port, _ := net.SplitHostPort(c.targetAddress)
		if host != "" {
			req.Host = host
		}
		if port != "" && port != "80" {
			req.Host += fmt.Sprintf(":%s", port)
		}

		var afterBuf bytes.Buffer
		req.Write(&afterBuf)

		// Replace request bytes with updated
		// TODO(adam): We need a more performant solution...
		b = bytes.Replace(b, beforeBuf.Bytes(), afterBuf.Bytes(), 1)
	}

read:
	if shouldFail(c.readFailureRatio) {
		partial := len(b) / 2
		_, err := c.Conn.Read(b[:partial])
		if err != nil {
			return partial, io.ErrShortWrite
		}
		return partial, io.ErrUnexpectedEOF
	}

	return c.Conn.Read(b)
}

func (c *conn) Write(b []byte) (n int, err error) {
	if shouldFail(c.writeFailureRatio) {
		partial := len(b) / 2
		_, err := c.Conn.Write(b[:partial])
		if err != nil {
			return partial, io.ErrShortWrite
		}
		return partial, io.ErrUnexpectedEOF
	}

	return c.Conn.Write(b)
}

type listener struct {
	throttled     *throttle.Listener
	targetAddress string

	readFailureRatio  int // 1-100%
	writeFailureRatio int // 1-100%
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.throttled.Accept()
	if err != nil {
		return nil, fmt.Errorf("listener.Accept: %w", err)
	}
	return &conn{
		Conn:              c,
		targetAddress:     l.targetAddress,
		readFailureRatio:  l.readFailureRatio,
		writeFailureRatio: l.writeFailureRatio,
	}, nil
}

func (l *listener) Close() error {
	return l.throttled.Close()
}

func (l *listener) Addr() net.Addr {
	return l.throttled.Addr()
}

func newListener(conf Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", conf.Listen)
	if err != nil {
		return nil, fmt.Errorf("newListener: %w", err)
	}

	throttled := &throttle.Listener{
		Listener: ln,
		Down: throttle.Rate{
			KBps:    conf.Read.MaxKBps,
			Latency: conf.Read.Latency,
		},
		Up: throttle.Rate{
			KBps:    conf.Write.MaxKBps,
			Latency: conf.Write.Latency,
		},
	}

	return &listener{
		throttled:         throttled,
		targetAddress:     conf.targetAddress(),
		readFailureRatio:  conf.Read.FailureRatio,
		writeFailureRatio: conf.Write.FailureRatio,
	}, nil
}

func pipe(errCh chan error, dst, src io.ReadWriter, counter *atomic.Uint32) {
	var count sync.Once
	for {
		_, err := io.Copy(dst, src)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				count.Do(func() {
					counter.Add(1)
				})
			}
		}
		errCh <- err
	}
}
