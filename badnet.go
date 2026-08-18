package badnet

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const defaultDialTimeout = 5 * time.Second

// Config describes a test proxy. Read and Write are from the connecting
// client's point of view: Read impairs data the client receives, Write
// impairs data the client sends.
type Config struct {
	Listen      string // local address, e.g. "127.0.0.1:0"
	Target      string // upstream host:port or URL
	Read        Direction
	Write       Direction
	DialTimeout time.Duration // 0 uses a 5s default
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

// Direction impairs one direction of traffic, from the connecting client's
// point of view. FailureRatio is the percent (0-100) of connections that
// fail in this direction. Latency is applied once, on the first I/O.
// MaxKBps of 0 means unlimited.
type Direction struct {
	MaxKBps      int
	Latency      time.Duration
	FailureRatio int
}

func (d Direction) active() bool {
	return d.MaxKBps > 0 || d.Latency > 0 || d.FailureRatio > 0
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

	connectionCount   atomic.Uint32
	failedConnections atomic.Uint32
	readFailures      atomic.Uint32
	writeFailures     atomic.Uint32
	targetFailures    atomic.Uint32
}

// ForTest starts a proxy that shuts down when the test ends.
// Listen errors fail the test; later target dial errors do not.
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
	ln, err := net.Listen("tcp", conf.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
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
		p.failedConnections.Add(1)
		return
	}
	defer target.Close()
	p.addConn(target)

	client = wrapConn(client, p.conf)

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
	err2 := <-errc

	if (err1 != nil && !isBenign(err1)) || (err2 != nil && !isBenign(err2)) {
		p.failedConnections.Add(1)
	}
}

// BindAddr is the host:port the application should dial.
func (p *Proxy) BindAddr() string {
	return p.bindAddr
}

// Port is the listening port, or -1 if BindAddr cannot be parsed.
func (p *Proxy) Port() int {
	_, port, err := net.SplitHostPort(p.bindAddr)
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return -1
	}
	return int(n)
}

// FailureRatio is the fraction of accepted connections that had a dial
// failure or an injected/copy failure.
func (p *Proxy) FailureRatio() float64 {
	connections := float64(p.connectionCount.Load())
	if connections == 0 {
		return 0
	}
	return float64(p.failedConnections.Load()) / connections
}

type closeWriter interface {
	CloseWrite() error
}

type conn struct {
	net.Conn
	toClient       Direction
	fromClient     Direction
	failToClient   atomic.Bool
	failFromClient atomic.Bool
	toClientOnce   sync.Once
	fromClientOnce sync.Once
}

func wrapConn(c net.Conn, conf Config) net.Conn {
	if !conf.Read.active() && !conf.Write.active() {
		return c
	}
	wc := &conn{
		Conn:       c,
		toClient:   conf.Read,
		fromClient: conf.Write,
	}
	if shouldFail(conf.Read.FailureRatio) {
		wc.failToClient.Store(true)
	}
	if shouldFail(conf.Write.FailureRatio) {
		wc.failFromClient.Store(true)
	}
	return wc
}

func shouldFail(ratio int) bool {
	if ratio <= 0 {
		return false
	}
	if ratio >= 100 {
		return true
	}
	return rand.IntN(100) < ratio
}

func (c *conn) Read(b []byte) (int, error) {
	c.fromClientOnce.Do(func() { sleep(c.fromClient.Latency) })
	if c.failFromClient.CompareAndSwap(true, false) {
		return c.partialRead(b)
	}
	if c.fromClient.MaxKBps > 0 && len(b) > 1024 {
		b = b[:1024]
	}
	n, err := c.Conn.Read(b)
	throttle(n, c.fromClient.MaxKBps)
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	c.toClientOnce.Do(func() { sleep(c.toClient.Latency) })
	if c.failToClient.CompareAndSwap(true, false) {
		return c.partialWrite(b)
	}
	if c.toClient.MaxKBps <= 0 {
		return c.Conn.Write(b)
	}
	var written int
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 1024 {
			chunk = chunk[:1024]
		}
		throttle(len(chunk), c.toClient.MaxKBps)
		n, err := c.Conn.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		if n < len(chunk) {
			return written, io.ErrShortWrite
		}
		b = b[n:]
	}
	return written, nil
}

func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *conn) partialRead(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	throttle(n, c.fromClient.MaxKBps)
	if err != nil {
		return n, err
	}
	keep := partialLen(n)
	return keep, io.ErrUnexpectedEOF
}

func (c *conn) partialWrite(b []byte) (int, error) {
	chunk := b[:partialLen(len(b))]
	throttle(len(chunk), c.toClient.MaxKBps)
	n, err := c.Conn.Write(chunk)
	if err != nil {
		return n, err
	}
	return n, io.ErrShortWrite
}

// readerOnly / writerOnly hide net.TCPConn's WriteTo/ReadFrom so io.Copy
// always goes through our Read/Write hooks.
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

func sleep(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

func throttle(n, kbps int) {
	if kbps <= 0 || n <= 0 {
		return
	}
	time.Sleep(time.Second * time.Duration(n) / time.Duration(kbps*1024))
}

func partialLen(n int) int {
	if n <= 1 {
		return n
	}
	return n / 2
}
