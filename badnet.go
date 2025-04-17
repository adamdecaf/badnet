package badnet

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go4.org/net/throttle"
)

type Config struct {
	Listen, Target string
	Read           Direction
	Write          Direction
}

func (c Config) targetAddress() string {
	host := c.Target
	port := "80"

	if u, err := url.Parse(c.Target); err == nil && u.Host != "" {
		host = u.Host
	}

	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		if p != "" {
			port = p
		}
	}

	return net.JoinHostPort(host, port)
}

type Direction struct {
	MaxKBps      int
	Latency      time.Duration
	FailureRatio int
}

type Proxy struct {
	conf           Config
	bindAddr       string
	listener       net.Listener
	listenerClosed chan struct{}

	connectionCount atomic.Uint32
	readFailures    atomic.Uint32
	writeFailures   atomic.Uint32
	targetFailures  atomic.Uint32
}

func ForTest(t *testing.T, conf Config) *Proxy {
	t.Helper()

	p := &Proxy{
		conf:           conf,
		listenerClosed: make(chan struct{}),
	}

	ln, err := newListener(p.conf)
	if err != nil {
		t.Fatalf("badnet listen failed: %v", err)
	}
	p.listener = ln
	p.bindAddr = ln.Addr().String()

	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		ln.Close()
		cancel()
		<-p.listenerClosed
	})

	go func() {
		defer close(p.listenerClosed)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Error("badnet listener accept error:", err)
				}
				return
			}
			p.connectionCount.Add(1)

			go func(conn net.Conn) {
				defer conn.Close()

				target, err := net.Dial("tcp", p.conf.targetAddress())
				if err != nil {
					p.targetFailures.Add(1)
					t.Error("connecting to", p.conf.targetAddress(), "failed:", err)
					return
				}
				defer target.Close()

				errCh := make(chan error, 2)
				go pipe(errCh, conn, target, &p.readFailures)
				go pipe(errCh, target, conn, &p.writeFailures)

				<-errCh
			}(conn)
		}
	}()

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

func (p *Proxy) FailureRatio() float64 {
	connections := float64(p.connectionCount.Load())
	failures := float64(p.readFailures.Load() + p.writeFailures.Load() + p.targetFailures.Load())
	if connections == 0 {
		return 0
	}
	return failures / connections
}

type conn struct {
	net.Conn
	readFailureRatio  int
	writeFailureRatio int
}

var maxChoice = big.NewInt(100)

func shouldFail(ratio int) bool {
	if ratio <= 0 {
		return false
	}
	n, _ := rand.Int(rand.Reader, maxChoice)
	return n.Int64() < int64(ratio)
}

func (c *conn) Read(b []byte) (n int, err error) {
	if shouldFail(c.readFailureRatio) {
		n, err = c.Conn.Read(b[:len(b)/2])
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	return c.Conn.Read(b)
}

func (c *conn) Write(b []byte) (n int, err error) {
	if shouldFail(c.writeFailureRatio) {
		n, err = c.Conn.Write(b[:len(b)/2])
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	return c.Conn.Write(b)
}

type listener struct {
	throttled         *throttle.Listener
	readFailureRatio  int
	writeFailureRatio int
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

func pipe(errCh chan error, dst, src io.ReadWriter, counter *atomic.Uint32) {
	_, err := io.Copy(dst, src)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		counter.Add(1)
	}
	errCh <- err
}
