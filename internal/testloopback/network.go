package testloopback

import (
	"net"
	"sync"
	"time"
)

const loopbackIPv4Address = "127.0.0.1"

var loopbackIPv4 = net.ParseIP(loopbackIPv4Address).To4()

type TCPListener struct {
	listener  net.Listener
	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

func (listener *TCPListener) Accept() (net.Conn, error) { return listener.listener.Accept() }

func (listener *TCPListener) Addr() net.Addr { return listener.listener.Addr() }

func (listener *TCPListener) Close() error {
	if listener == nil {
		return nil
	}
	listener.closeOnce.Do(func() {
		listener.closeErr = listener.listener.Close()
		close(listener.closed)
	})
	return listener.closeErr
}

func (listener *TCPListener) Closed() <-chan struct{} {
	if listener == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return listener.closed
}

type UDPConn struct {
	connection *net.UDPConn
	closeOnce  sync.Once
	closeErr   error
	closed     chan struct{}
}

func (connection *UDPConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	return connection.connection.ReadFrom(buffer)
}

func (connection *UDPConn) ReadFromUDP(buffer []byte) (int, *net.UDPAddr, error) {
	return connection.connection.ReadFromUDP(buffer)
}

func (connection *UDPConn) WriteTo(buffer []byte, address net.Addr) (int, error) {
	return connection.connection.WriteTo(buffer, address)
}

func (connection *UDPConn) WriteToUDP(buffer []byte, address *net.UDPAddr) (int, error) {
	return connection.connection.WriteToUDP(buffer, address)
}

func (connection *UDPConn) LocalAddr() net.Addr { return connection.connection.LocalAddr() }

func (connection *UDPConn) SetDeadline(deadline time.Time) error {
	return connection.connection.SetDeadline(deadline)
}

func (connection *UDPConn) SetReadDeadline(deadline time.Time) error {
	return connection.connection.SetReadDeadline(deadline)
}

func (connection *UDPConn) SetWriteDeadline(deadline time.Time) error {
	return connection.connection.SetWriteDeadline(deadline)
}

func (connection *UDPConn) Close() error {
	if connection == nil {
		return nil
	}
	connection.closeOnce.Do(func() {
		connection.closeErr = connection.connection.Close()
		close(connection.closed)
	})
	return connection.closeErr
}

func (connection *UDPConn) Closed() <-chan struct{} {
	if connection == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return connection.closed
}

func (fixture *Fixture) ListenTCP() *TCPListener {
	fixture.t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(loopbackIPv4Address, "0"))
	if err != nil {
		fixture.t.Fatalf("listen on TCP loopback: %v", err)
	}
	owned := &TCPListener{listener: listener, closed: make(chan struct{})}
	if err := fixture.own("TCP listener "+listener.Addr().String(), owned); err != nil {
		_ = owned.Close()
		fixture.t.Fatalf("own TCP loopback listener: %v", err)
	}
	return owned
}

func (fixture *Fixture) ListenUDP() *UDPConn {
	fixture.t.Helper()
	connection, err := listenUDP()
	if err != nil {
		fixture.t.Fatalf("listen on UDP loopback: %v", err)
	}
	if err := fixture.own("UDP packet connection "+connection.LocalAddr().String(), connection); err != nil {
		_ = connection.Close()
		fixture.t.Fatalf("own UDP loopback packet connection: %v", err)
	}
	return connection
}

func listenUDP() (*UDPConn, error) {
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopbackIPv4, Port: 0})
	if err != nil {
		return nil, err
	}
	return &UDPConn{connection: connection, closed: make(chan struct{})}, nil
}
