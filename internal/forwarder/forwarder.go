// Package forwarder is a forward-only DNS proxy. It copies every query to
// a configured upstream and returns the upstream's answer to the client
// byte-for-byte: no blocklist, no answer rewriting, no NXDOMAIN injection,
// no DNSSEC stripping, and no record type is special-cased on the wire
// path. A query it cannot even parse is still forwarded unchanged.
//
// Logging is a side channel. The client always gets its answer first;
// recording the hostname happens after the response is already on the
// wire, through a logger that is documented never to block.
package forwarder

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"time"

	"github.com/miekg/dns"
)

// Logger receives one entry per observed query. Implementations (see
// internal/store) must not block or fail the caller; the forwarder does
// not wait on them and does not retry them.
type Logger interface {
	LogQuery(hostname string, qtype uint16, at time.Time)
}

// NoopLogger discards every event. Useful for tests and as a safe default.
type NoopLogger struct{}

func (NoopLogger) LogQuery(string, uint16, time.Time) {}

// Server forwards DNS queries received on UDP and TCP port Addr to
// Upstream, unmodified.
type Server struct {
	// Addr is the local address to listen on, e.g. ":53".
	Addr string
	// Upstream is the resolver to forward every query to, e.g.
	// "1.1.1.1:53".
	Upstream string
	// Logger records each observed query. Defaults to NoopLogger if nil.
	Logger Logger
	// Timeout bounds each upstream round trip. Defaults to 5s if zero.
	Timeout time.Duration
	// MaxConcurrentUDP bounds how many UDP queries are in flight to the
	// upstream at once, and therefore how many goroutines and upstream
	// sockets the forwarder can ever hold open. Defaults to 256 if zero.
	// A query received beyond the cap is never dropped or answered
	// locally - the accept loop blocks acquiring a slot, so excess
	// datagrams simply queue in the kernel's UDP receive buffer, the same
	// backpressure any UDP server exhibits under load.
	MaxConcurrentUDP int

	udpConn net.PacketConn
	tcpLn   net.Listener
	udpSem  chan struct{}
	udpPool chan net.Conn
}

const maxUDPSize = 65535
const defaultMaxConcurrentUDP = 256

// ListenAndServe binds the UDP and TCP listeners, then blocks forwarding
// queries until one of them fails. It closes both listeners before
// returning. Equivalent to calling Listen followed by Serve.
func (s *Server) ListenAndServe() error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve()
}

// Listen applies defaults and binds the UDP and TCP sockets, returning
// once both are ready to accept queries. It does not block serving them -
// call Serve for that.
//
// Splitting bind from serve gives callers (tests, mainly) a real
// synchronization point: every field Close reads (udpConn, tcpLn,
// udpPool) is written here, in the caller's own goroutine, before Serve
// is ever started. A caller that waits for Listen to return before
// spawning `go s.Serve()` and concurrently calling Close needs no other
// signal - the goroutine-creation boundary alone establishes the
// happens-before relationship. The previous single ListenAndServe forced
// callers that wanted this concurrently (e.g. for shutdown) to guess at a
// safe delay, which raced Close against these same field writes.
func (s *Server) Listen() error {
	if s.Timeout == 0 {
		s.Timeout = 5 * time.Second
	}
	if s.Logger == nil {
		s.Logger = NoopLogger{}
	}
	if s.MaxConcurrentUDP <= 0 {
		s.MaxConcurrentUDP = defaultMaxConcurrentUDP
	}
	s.udpSem = make(chan struct{}, s.MaxConcurrentUDP)
	s.udpPool = make(chan net.Conn, s.MaxConcurrentUDP)

	udpConn, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		return err
	}
	s.udpConn = udpConn

	tcpLn, err := net.Listen("tcp", s.Addr)
	if err != nil {
		udpConn.Close()
		return err
	}
	s.tcpLn = tcpLn
	return nil
}

// Serve forwards queries on the sockets bound by Listen until one of them
// fails, then closes both before returning. Listen must have already
// completed successfully.
func (s *Server) Serve() error {
	errCh := make(chan error, 2)
	go func() { errCh <- s.serveUDP(s.udpConn) }()
	go func() { errCh <- s.serveTCP(s.tcpLn) }()

	err := <-errCh
	s.Close()
	return err
}

// Close stops both listeners and releases any pooled upstream sockets.
// ListenAndServe returns once both loops have unwound.
func (s *Server) Close() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.tcpLn != nil {
		s.tcpLn.Close()
	}
	for {
		select {
		case c := <-s.udpPool:
			c.Close()
		default:
			return
		}
	}
}

func (s *Server) serveUDP(conn net.PacketConn) error {
	buf := make([]byte, maxUDPSize)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		query := make([]byte, n)
		copy(query, buf[:n])

		// Blocks if MaxConcurrentUDP queries are already in flight,
		// rather than spawning an unbounded goroutine per query. The
		// datagram has already been read off the client socket, so
		// this never drops or rewrites it - it only delays picking up
		// the *next* datagram until a slot frees up.
		s.udpSem <- struct{}{}
		go func() {
			defer func() { <-s.udpSem }()
			s.handleUDP(conn, addr, query)
		}()
	}
}

func (s *Server) handleUDP(conn net.PacketConn, addr net.Addr, query []byte) {
	resp, err := s.forwardUDP(query)
	if err != nil {
		log.Printf("forwarder: upstream UDP forward failed: %v", err)
		return
	}
	if _, err := conn.WriteTo(resp, addr); err != nil {
		log.Printf("forwarder: writing UDP response to client failed: %v", err)
		return
	}
	s.logQuery(query)
}

// forwardUDP borrows a connected upstream socket from the pool (dialing a
// new one only when the pool is empty), uses it for exactly one
// query/response round trip, and returns it to the pool for reuse. The
// pool never holds more than MaxConcurrentUDP connections because at most
// that many goroutines ever call this concurrently, so the forwarder's
// total open upstream sockets are bounded the same way its goroutines are.
func (s *Server) forwardUDP(query []byte) ([]byte, error) {
	conn, err := s.borrowUpstreamUDP()
	if err != nil {
		return nil, err
	}

	conn.SetDeadline(time.Now().Add(s.Timeout))
	if _, err := conn.Write(query); err != nil {
		conn.Close()
		return nil, err
	}

	// A reused socket can still have a stale or duplicate response to a
	// *previous* query sitting in its receive buffer (e.g. a slow
	// upstream answering just after a prior round trip already timed
	// out and the goroutine moved on). The DNS header's 2-byte
	// transaction ID is the only way to tell that response apart from
	// the one this call is actually waiting for, so every read is
	// checked against the query's own ID and anything else is
	// discarded without being relayed - never to the wrong client,
	// and bounded by the same deadline as the rest of the round trip.
	hasID := len(query) >= 2
	var wantID uint16
	if hasID {
		wantID = binary.BigEndian.Uint16(query[:2])
	}

	buf := make([]byte, maxUDPSize)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if hasID && (n < 2 || binary.BigEndian.Uint16(buf[:2]) != wantID) {
			continue
		}
		s.returnUpstreamUDP(conn)
		return buf[:n], nil
	}
}

func (s *Server) borrowUpstreamUDP() (net.Conn, error) {
	select {
	case conn := <-s.udpPool:
		return conn, nil
	default:
		return net.DialTimeout("udp", s.Upstream, s.Timeout)
	}
}

func (s *Server) returnUpstreamUDP(conn net.Conn) {
	select {
	case s.udpPool <- conn:
	default:
		// Pool is unexpectedly full (shouldn't happen given it is
		// sized to MaxConcurrentUDP); don't leak the socket.
		conn.Close()
	}
}

func (s *Server) serveTCP(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleTCP(conn)
	}
}

func (s *Server) handleTCP(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(s.Timeout))

	query, err := readTCPMessage(conn)
	if err != nil {
		if err != io.EOF {
			log.Printf("forwarder: reading TCP query from client failed: %v", err)
		}
		return
	}

	resp, err := s.forwardTCP(query)
	if err != nil {
		log.Printf("forwarder: upstream TCP forward failed: %v", err)
		return
	}

	if err := writeTCPMessage(conn, resp); err != nil {
		log.Printf("forwarder: writing TCP response to client failed: %v", err)
		return
	}
	s.logQuery(query)
}

func (s *Server) forwardTCP(query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", s.Upstream, s.Timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(s.Timeout))

	if err := writeTCPMessage(conn, query); err != nil {
		return nil, err
	}
	return readTCPMessage(conn)
}

// readTCPMessage reads one length-prefixed DNS message per RFC 1035
// section 4.2.2.
func readTCPMessage(r io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeTCPMessage(w io.Writer, msg []byte) error {
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(msg)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

// logQuery best-effort parses the original query to extract the question
// name and type for logging. The response has already been written to the
// client by the time this runs; a query this parser cannot decode is
// simply not logged — it was still forwarded unchanged on the wire.
func (s *Server) logQuery(query []byte) {
	msg := new(dns.Msg)
	if err := msg.Unpack(query); err != nil {
		return
	}
	if len(msg.Question) == 0 {
		return
	}
	q := msg.Question[0]
	s.Logger.LogQuery(q.Name, q.Qtype, time.Now())
}
