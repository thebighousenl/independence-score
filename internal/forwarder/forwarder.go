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

	udpConn net.PacketConn
	tcpLn   net.Listener
}

const maxUDPSize = 65535

// ListenAndServe starts the UDP and TCP listeners and blocks, forwarding
// queries until one of the listeners fails. It closes both listeners
// before returning.
func (s *Server) ListenAndServe() error {
	if s.Timeout == 0 {
		s.Timeout = 5 * time.Second
	}
	if s.Logger == nil {
		s.Logger = NoopLogger{}
	}

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

	errCh := make(chan error, 2)
	go func() { errCh <- s.serveUDP(udpConn) }()
	go func() { errCh <- s.serveTCP(tcpLn) }()

	err = <-errCh
	s.Close()
	return err
}

// Close stops both listeners. ListenAndServe returns once both loops have
// unwound.
func (s *Server) Close() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.tcpLn != nil {
		s.tcpLn.Close()
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
		go s.handleUDP(conn, addr, query)
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

func (s *Server) forwardUDP(query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp", s.Upstream, s.Timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(s.Timeout))
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}

	buf := make([]byte, maxUDPSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
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
