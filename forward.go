// Package forward contains a UDP packet forwarder.
package forward

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	bufferSize = 4096
	// defaultTimeout is the default timeout period of inactivity for convenience
	// sake. It is equivelant to 5 minutes.
	defaultTimeout = time.Minute * 5
)

type connection struct {
	available  chan struct{}
	udp        *net.UDPConn
	lastActive time.Time
	cancel     context.CancelFunc
}

type Forwarder interface {
	Start(context.Context)
	Close()
	Connected() []string
	Update(...Option) error
	Destinations() []net.UDPAddr
}

type Destination interface {
	Name() string
	Addr() net.UDPAddr
	String() string
}

// Forwarder represents a UDP packet forwarder.
type forwarder struct {
	src          *net.UDPAddr
	dst          []destination
	dstMutex     *sync.RWMutex
	client       *net.UDPAddr
	listenerConn *net.UDPConn

	connections      map[string]*connection
	connectionsMutex *sync.RWMutex

	connectCallback    func(addr string)
	disconnectCallback func(addr string)

	broadcast bool

	timeout time.Duration

	closedCh chan struct{}
}

type destination struct {
	name string
	addr *net.UDPAddr
}

// function options
type Option func(*forwarder) error

func WithTimeout(timeout time.Duration) Option {
	return func(f *forwarder) error {
		f.timeout = timeout
		return nil
	}
}

func WithConnectCallback(callback func(addr string)) Option {
	return func(f *forwarder) error {
		f.connectCallback = callback
		return nil
	}
}

func WithDisconnectCallback(callback func(addr string)) Option {
	return func(f *forwarder) error {
		f.disconnectCallback = callback
		return nil
	}
}

func WithDestination(name, addr string) Option {
	return func(f *forwarder) error {
		udpDddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return err
		}

		f.dst = append(f.dst,
			destination{
				name: name,
				addr: udpDddr,
			})
		return nil
	}
}

func WithBroadcast() Option {
	return func(f *forwarder) error {
		f.broadcast = true
		return nil
	}
}

func NewForwarder(src string, args ...Option) (*forwarder, error) {
	udpSrc, err := net.ResolveUDPAddr("udp", src)
	if err != nil {
		return nil, err
	}

	listenerConn, err := net.ListenUDP("udp", udpSrc)
	if err != nil {
		return nil, err
	}

	f := &forwarder{
		connectCallback:    func(addr string) {},
		disconnectCallback: func(addr string) {},
		connectionsMutex:   new(sync.RWMutex),
		connections:        make(map[string]*connection),
		timeout:            defaultTimeout,
		listenerConn:       listenerConn,
		src:                udpSrc,
		dstMutex:           new(sync.RWMutex),
		closedCh:           make(chan struct{}),
	}

	for _, setter := range args {
		if err := setter(f); err != nil {
			return nil, err
		}
	}

	return f, nil
}

// Start forwards UDP packets from the src address to the dst address, with a
// timeout to "disconnect" clients after the timeout period of inactivity. It
// implements a reverse NAT and thus supports multiple seperate users. Forward
// is also asynchronous.
func (f *forwarder) Start(ctx context.Context) {
	// Start the janitor
	go f.janitor()

	for {
		buf := make([]byte, bufferSize)
		oob := make([]byte, bufferSize)

		n, _, _, from, err := f.listenerConn.ReadMsgUDP(buf, oob)
		if err != nil {
			slog.Error("forward: failed to read, terminating", err)
			return
		}
		ctx, cancel := context.WithCancel(ctx)

		go f.handle(ctx, cancel, buf[:n], from)
	}
}

// Close stops the forwarder.
func (f *forwarder) Close() {
	f.connectionsMutex.Lock()
	defer f.connectionsMutex.Unlock()

	f.closedCh <- struct{}{}
	for _, conn := range f.connections {
		conn.udp.Close()
	}
	f.listenerConn.Close()
}

// Connected returns the list of connected clients in IP:port form.
func (f *forwarder) Connected() []string {
	f.connectionsMutex.RLock()
	defer f.connectionsMutex.RUnlock()
	results := make([]string, 0, len(f.connections))
	for key := range f.connections {
		results = append(results, key)
	}
	return results
}

// Update sets a new destination list
func (f *forwarder) Update(opts ...Option) error {
	var t = &forwarder{}
	for _, setter := range opts {
		if err := setter(t); err != nil {
			return err
		}
	}

	f.dstMutex.Lock()
	defer f.dstMutex.Unlock()
	f.dst = t.dst

	return nil
}

func (f *forwarder) Destinations() []Destination {
	f.dstMutex.RLock()
	defer f.dstMutex.RUnlock()
	var res []Destination
	for _, d := range f.dst {
		res = append(res, d)
	}
	return res
}

func (f *forwarder) janitor() {
	for {
		select {
		case <-f.closedCh:
			return
		case <-time.After(f.timeout):
			var disconnectCallbacks []func()

			f.connectionsMutex.Lock()
			for addr, conn := range f.connections {
				if conn.lastActive.Before(time.Now().Add(-f.timeout)) {
					slog.Debug("udp-forward: timed out, closing", "addr", addr)
					conn.cancel()
					conn.udp.Close()
					disconnectCallbacks = append(disconnectCallbacks,
						func() {
							f.disconnectCallback(addr)
						})
					delete(f.connections, addr)
				}
			}
			f.connectionsMutex.Unlock()

			for _, callback := range disconnectCallbacks {
				callback()
			}
		}
	}
}

func (f *forwarder) handle(ctx context.Context,
	cancel context.CancelFunc,
	data []byte,
	from *net.UDPAddr,
) {
	f.connectionsMutex.Lock()
	conn, found := f.connections[from.String()]
	if !found {
		f.connections[from.String()] = &connection{
			available:  make(chan struct{}),
			udp:        nil,
			lastActive: time.Now(),
			cancel:     cancel,
		}
	}
	f.connectionsMutex.Unlock()

	if !found {
		var (
			udpConn *net.UDPConn
			laddr   *net.UDPAddr
		)

		// pick out a random destination
		dst := f.getDestination()

		if dst.addr.IP.To4()[0] == 127 {
			slog.Debug("using local listener")
			laddr, _ = net.ResolveUDPAddr("udp", "127.0.0.1:")
		}

		slog.Debug("dialing", "dst", dst.String())
		udpConn, err := net.DialUDP("udp", laddr, dst.addr)
		if err != nil {
			slog.Error("udp-forward: failed to dial", err)
			delete(f.connections, from.String())
			return
		}

		f.connectionsMutex.Lock()
		f.connections[from.String()].udp = udpConn
		f.connections[from.String()].lastActive = time.Now()
		close(f.connections[from.String()].available)
		f.connectionsMutex.Unlock()

		f.connectCallback(from.String())

		if _, _, err := udpConn.WriteMsgUDP(data, nil, nil); err != nil {
			slog.Error("udp-forward: error sending initial packet to client", err)
		}

		for {
			buf := make([]byte, bufferSize)
			oob := make([]byte, bufferSize)

			n, _, _, _, err := udpConn.ReadMsgUDP(buf, oob)
			if err != nil {
				f.connectionsMutex.Lock()
				udpConn.Close()
				delete(f.connections, from.String())
				f.connectionsMutex.Unlock()

				// if the context got cancelled, that means
				// the janitor closed the connection due to inactivity
				if err := ctx.Err(); err != nil {
					return
				}

				f.disconnectCallback(from.String())
				if !strings.Contains(err.Error(), "use of closed network connection") {
					slog.Error("udp-forward: abnormal read, closing", "error", err)
				}
				return
			}

			if _, _, err := f.listenerConn.WriteMsgUDP(buf[:n], nil, from); err != nil {
				slog.Error("udp-forward: error sending packet to client", "error", err)
			}
		}

		// unreachable
	}

	<-conn.available

	// log.Println("sent packet to server", conn.udp.RemoteAddr())
	_, _, err := conn.udp.WriteMsgUDP(data, nil, nil)
	if err != nil {
		slog.Error("udp-forward: error sending packet to server:", err)
	}

	shouldChangeTime := false
	f.connectionsMutex.RLock()
	if _, found := f.connections[from.String()]; found {
		if f.connections[from.String()].lastActive.Before(
			time.Now().Add(f.timeout / 4)) {
			shouldChangeTime = true
		}
	}
	f.connectionsMutex.RUnlock()

	if shouldChangeTime {
		f.connectionsMutex.Lock()
		// Make sure it still exists
		if _, found := f.connections[from.String()]; found {
			connWrapper := f.connections[from.String()]
			connWrapper.lastActive = time.Now()
			f.connections[from.String()] = connWrapper
		}
		f.connectionsMutex.Unlock()
	}
}

func (f *forwarder) getDestination() destination {
	f.dstMutex.RLock()
	defer f.dstMutex.RUnlock()

	return f.dst[rand.Intn(len(f.dst))]
}

func (d destination) Name() string {
	return d.name
}

func (d destination) Addr() net.UDPAddr {
	return *d.addr
}

func (d destination) String() string {
	return fmt.Sprintf("%s (%s)",
		d.name, d.addr.String())
}
