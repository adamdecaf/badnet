package badnet

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go4.org/net/throttle"
)

const defaultDialTimeout = 5 * time.Second

type Config struct {
	Listen, Target string
	Read           Direction
	Write          Direction
	DialTimeout    time.Duration
}

func (c Config) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return defaultDialTimeout
}

func (c Config) targetAddress() string {
	host := c.Target
	port := "80"

	if u, err := url.Parse(c.Target); err == nil && u.Host != "" {
		host = u.Host
		switch strings.ToLower(u.Scheme) {
		case "https", "wss":
			port = "443"
		}
	}

	return joinHostPortDefault(host, port)
}

func joinHostPortDefault(host, defaultPort string) string {
	if h, p, err := net.SplitHostPort(host); err == nil {
		if p != "" {
			return net.JoinHostPort(h, p)
		}
		return net.JoinHostPort(h, defaultPort)
	}
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	return net.JoinHostPort(host, defaultPort)
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
	wg             sync.WaitGroup
	closing        atomic.Bool

	mu    sync.Mutex
	conns []net.Conn

	connectionCount atomic.Uint32
	readFailures    atomic.Uint32
	writeFailures   atomic.Uint32
	targetFailures  atomic.Uint32
}

func ForTest(t *testing.T, conf Config) *Proxy {
	t.Helper()

	p, err := newProxy(conf)
	if err != nil {
		t.Fatalf("badnet listen failed: %v", err)
	}

	t.Cleanup(p.shutdown)
	go p.serve(p.listener)
	return p
}

func newProxy(conf Config) (*Proxy, error) {
	ln, err := newListener(conf)
	if err != nil {
		return nil, err
	}
	return &Proxy{
		conf:           conf,
		listener:       ln,
		bindAddr:       ln.Addr().String(),
		listenerClosed: make(chan struct{}),
	}, nil
}

func (p *Proxy) shutdown() {
	p.closing.Store(true)
	if p.listener != nil {
		p.listener.Close()
	}
	<-p.listenerClosed
	p.mu.Lock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *Proxy) addConn(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

func (p *Proxy) serve(ln net.Listener) {
	defer close(p.listenerClosed)

	var delay time.Duration
	for {
		accepted, err := ln.Accept()
		if err != nil {
			if retryableAccept(err) {
				if delay == 0 {
					delay = 5 * time.Millisecond
				}
				time.Sleep(delay)
				if delay < 80*time.Millisecond {
					delay *= 2
				}
				continue
			}
			return
		}
		delay = 0
		p.wg.Add(1)
		go p.handle(accepted)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	p.addConn(client)

	p.connectionCount.Add(1)
	if p.closing.Load() {
		return
	}

	target, err := net.DialTimeout("tcp", p.conf.targetAddress(), p.conf.dialTimeout())
	if err != nil {
		p.targetFailures.Add(1)
		return
	}
	defer target.Close()
	p.addConn(target)

	errc := make(chan error, 2)
	go func() {
		errc <- copyDir(target, client, &p.writeFailures)
	}()
	go func() {
		errc <- copyDir(client, target, &p.readFailures)
	}()

	err1 := <-errc
	if err1 != nil && !isBenign(err1) {
		deadline := time.Now().Add(250 * time.Millisecond)
		_ = client.SetDeadline(deadline)
		_ = target.SetDeadline(deadline)
	}
	<-errc
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

func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
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

type closeWriter interface {
	CloseWrite() error
}

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

func copyDir(dst, src net.Conn, counter *atomic.Uint32) error {
	_, err := io.Copy(writerOnly{dst}, readerOnly{src})
	if err != nil && !isBenign(err) {
		counter.Add(1)
	}
	closeWrite(dst)
	return err
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func retryableAccept(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout() || ne.Temporary()
	}
	return false
}

func isBenign(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return true
	}
	return false
}
