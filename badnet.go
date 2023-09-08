package badnet

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"go4.org/net/throttle"
)

type Config struct {
	Listen, Target string

	Read  Direction
	Write Direction
}

type Direction struct {
	MaxKBps      int // set 0 for unlimited
	Latency      time.Duration
	FailureRatio int
}

type Proxy struct {
	bindAddr string
}

func ForTest(t *testing.T, conf Config) *Proxy {
	t.Helper()

	var p Proxy
	var err error

	// Setup listener
	ln, err := newListener(conf)
	if err != nil {
		t.Fatalf("badnet listen failed: %v", err)
	}
	p.bindAddr = ln.Addr().String()

	// Cycle through connections to proxy traffic
	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(func() { cancelFunc() })

	go func(ctx context.Context, ln net.Listener) { //nolint:staticcheck
		for {
			// Block while waiting for a connection
			connCh := make(chan net.Conn)
			go func() { //nolint:staticcheck
				conn, err := ln.Accept()
				if err != nil {
					t.Fatalf("badnet listener accept error: %v", err) //nolint:govet,staticcheck
					return
				}
				connCh <- conn
			}()

			select {
			case <-ctx.Done():
				close(connCh)
				return

			case conn := <-connCh:
				// Connect to the target
				target, err := net.Dial("tcp", conf.Target)
				if err != nil {
					t.Fatalf("connecting to %s failed: %v", conf.Target, err) //nolint:govet,staticcheck
				}

				// pipe between the listener and target in both directions
				errCh := make(chan error, 1)
				go pipe(errCh, conn, target)
				go pipe(errCh, target, conn)
				<-errCh

				// Cleanup after ourselves
				target.Close()
				conn.Close()
			}
		}
	}(ctx, ln)

	return &p
}

func (p *Proxy) BindAddr() string {
	return p.bindAddr
}

type conn struct {
	net.Conn

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
	if shouldFail(c.readFailureRatio) {
		partial := len(b) / 2
		_, err := c.Conn.Read(b[:partial])
		if err != nil {
			return partial, err
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
			return partial, err
		}
		return partial, io.ErrUnexpectedEOF
	}

	return c.Conn.Write(b)
}

type listener struct {
	throttled *throttle.Listener

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

	if (conf.Read.MaxKBps == 0 && conf.Read.Latency == 0*time.Second) ||
		(conf.Write.MaxKBps == 0 && conf.Write.Latency == 0*time.Second) {
		// Don't wrap listener with a throttler if it's disabled
		return ln, nil
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
		readFailureRatio:  conf.Read.FailureRatio,
		writeFailureRatio: conf.Write.FailureRatio,
	}, nil
}

func pipe(errCh chan error, dst, src io.ReadWriter) {
	for {
		_, err := io.Copy(dst, src)
		errCh <- err
	}
}
