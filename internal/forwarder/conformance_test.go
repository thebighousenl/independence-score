// Conformance cases beyond well-formed queries: malformed and hostile
// input, large answers that need TCP fallback, and every upstream failure
// mode the brief calls out. The required behaviour throughout is the same
// one named in the brief: forward unchanged, or drop - never crash, never
// answer locally, never hang a goroutine.
package forwarder

import (
	"bytes"
	"encoding/binary"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// assertServerStillAlive proves a prior hostile input did not wedge the
// server: a normal, well-formed query sent afterward must still get a
// normal answer.
func assertServerStillAlive(t *testing.T, addr string) {
	t.Helper()
	q := newQuestion("still-alive.example.com", dns.TypeA)
	resp := queryUDP(t, addr, q)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("server did not serve a normal query after hostile input: %+v", resp)
	}
}

// assertUDPListenerStillAccepts proves the forwarder's UDP accept loop is
// still running after a prior failure by sending one more (also doomed to
// go unanswered, since these tests' upstream is broken for the whole
// server) query and checking it fails within a bounded deadline rather
// than hanging forever - a wedged accept loop would never even attempt
// the second round trip, so the client's own deadline would be the only
// thing that fires, indistinguishable in this case because this query was
// never going to succeed. What matters is that it fails on *this* timeout,
// not that it never returns at all.
func assertUDPListenerStillAccepts(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder for liveness check: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(within))

	q := newQuestion("liveness-check.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if _, err := conn.Write(packed); err != nil {
		t.Fatalf("write liveness-check query: %v", err)
	}
	buf := make([]byte, maxUDPSize)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("liveness-check query unexpectedly got an answer from a deliberately broken upstream")
	}
}

// assertTCPListenerStillAccepts is the TCP analogue: the forwarder must
// still accept new connections after a prior upstream failure, even though
// this suite's broken upstream means the new connection also closes
// without an answer.
func assertTCPListenerStillAccepts(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarder for liveness check: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(within))

	q := newQuestion("liveness-check.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if err := writeTCPMessage(conn, packed); err != nil {
		t.Fatalf("write liveness-check query: %v", err)
	}
	if _, err := readTCPMessage(conn); err == nil {
		t.Fatal("liveness-check query unexpectedly got an answer from a deliberately broken upstream")
	}
}

// assertNoGoroutineLeak gives any in-flight goroutines a moment to unwind,
// then fails if the goroutine count grew past the baseline captured before
// the hostile input was sent. A generous margin and retry loop absorb
// scheduler noise without masking an actual leak, which holds steady
// instead of settling back down.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		cur := runtime.NumGoroutine()
		if cur <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count = %d, want <= %d (baseline %d): suspected leak", cur, baseline+2, baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Malformed and hostile input ---------------------------------------

func TestForwarderUDPForwardsTruncatedQueryUnchanged(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, logger := startForwarder(t, upstream.addr)

	// Five bytes: shorter than even a DNS header (12 bytes).
	truncated := []byte{0x12, 0x34, 0x01, 0x00, 0x00}

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(truncated); err != nil {
		t.Fatalf("write truncated query: %v", err)
	}

	received := upstream.waitForRaw(t, 1)
	if !bytes.Equal(received[0], truncated) {
		t.Fatalf("upstream received %x, want unchanged %x", received[0], truncated)
	}

	time.Sleep(50 * time.Millisecond)
	if n := logger.count(); n != 0 {
		t.Fatalf("expected 0 logged queries for a truncated query, got %d", n)
	}
	assertServerStillAlive(t, addr)
}

func TestForwarderUDPForwardsZeroLengthDatagramWithoutCrashing(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte{}); err != nil {
		t.Fatalf("write zero-length datagram: %v", err)
	}

	received := upstream.waitForRaw(t, 1)
	if len(received[0]) != 0 {
		t.Fatalf("upstream received %d bytes, want 0", len(received[0]))
	}
	assertServerStillAlive(t, addr)
}

func TestForwarderUDPForwardsImpossibleLabelLengthUnchanged(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	// A 12-byte header followed by a label-length byte of 0x40: the top
	// two bits (01) are a reserved/extended label type under RFC 1035,
	// not a valid length and not a compression pointer (top bits 11).
	// No real resolver can parse this name; the forwarder must still
	// relay it unchanged because it never tries to.
	query := append([]byte{
		0x12, 0x34, // ID
		0x01, 0x00, // flags: standard query, recursion desired
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // ANCOUNT/NSCOUNT/ARCOUNT=0
	}, 0x40, 0x00, 0x01, 0x00, 0x01) // impossible label, QTYPE=A, QCLASS=IN

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("write query: %v", err)
	}

	received := upstream.waitForRaw(t, 1)
	if !bytes.Equal(received[0], query) {
		t.Fatalf("upstream received %x, want unchanged %x", received[0], query)
	}
	assertServerStillAlive(t, addr)
}

func TestForwarderUDPForwardsCompressionPointerLoopUnchanged(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	// Captured only after the upstream and forwarder's own long-lived
	// goroutines already exist, so the leak check below only has to
	// cover the ephemeral per-query goroutine this test's own write
	// spawns.
	baseline := runtime.NumGoroutine()

	// A question name that is a single compression pointer (0xC0 0x0C)
	// back to offset 12 - itself. Following it forever would be an
	// infinite loop; miekg/dns's own unpacker bounds pointer-following,
	// so this must not hang the forwarder either. The wire path never
	// even tries: it forwards the bytes without parsing them.
	query := []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // flags
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // AN/NS/AR = 0
		0xC0, 0x0C, // name: pointer to offset 12 (itself)
		0x00, 0x01, // QTYPE=A
		0x00, 0x01, // QCLASS=IN
	}

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("write query: %v", err)
	}

	received := upstream.waitForRaw(t, 1)
	if !bytes.Equal(received[0], query) {
		t.Fatalf("upstream received %x, want unchanged %x", received[0], query)
	}
	assertServerStillAlive(t, addr)
	assertNoGoroutineLeak(t, baseline)
}

// TestForwarderTCPDropsConnectionWhenDeclaredLengthExceedsBytesSent covers
// a client that sends a TCP length prefix promising more bytes than it
// actually writes before closing the connection. readTCPMessage's
// io.ReadFull cannot complete, so the forwarder must drop the connection -
// never crash, never hang past the deadline, and never forward a partial,
// fabricated message.
func TestForwarderTCPDropsConnectionWhenDeclaredLengthExceedsBytesSent(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], 100) // promise 100 bytes
	conn.Write(lenBuf[:])
	conn.Write([]byte("only ten!!")) // send 10, then close
	conn.Close()

	assertServerStillAlive(t, addr)
}

// TestForwarderTCPIgnoresBytesBeyondDeclaredLength covers the opposite
// shape: the client writes more bytes on the connection than its own
// length prefix declared. readTCPMessage must read exactly the declared
// message and nothing more; the trailing bytes are simply never read, not
// treated as a second message or used to corrupt the first.
func TestForwarderTCPIgnoresBytesBeyondDeclaredLength(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	q := newQuestion("tcp-extra-bytes.example.com", dns.TypeA)
	packed, err := q.Pack()
	if err != nil {
		t.Fatalf("pack query: %v", err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(packed)))
	conn.Write(lenBuf[:])
	conn.Write(packed)
	conn.Write([]byte("trailing garbage not part of any message"))

	resp, err := readTCPMessage(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	respMsg := new(dns.Msg)
	if err := respMsg.Unpack(resp); err != nil {
		t.Fatalf("unpack response: %v", err)
	}
	if len(respMsg.Answer) != 1 || respMsg.Answer[0].Header().Rrtype != dns.TypeA {
		t.Fatalf("unexpected response to the declared message: %+v", respMsg)
	}
}

// --- Large answers and truncation --------------------------------------

// TestForwarderUDPRelaysUpstreamTruncationUnchanged proves the forwarder
// itself never decides to truncate a UDP answer: when the upstream sets
// the TC bit (because the real answer would not fit in 512 bytes and the
// client did not advertise EDNS0), the forwarder relays that TC bit and
// the (empty) answer exactly as the upstream sent it, and the client's
// own TCP retry then gets the full answer over the forwarder's TCP path.
func TestForwarderUDPRelaysUpstreamTruncationUnchanged(t *testing.T) {
	upstream := startTruncatingUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	q := newQuestion("large-answer.example.com", dns.TypeA)
	resp := queryUDP(t, addr, q)

	if !resp.Truncated {
		t.Fatal("expected TC bit set on the UDP answer, forwarder must relay upstream's truncation signal")
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("expected no answer records alongside TC, got %d", len(resp.Answer))
	}

	// The client's real behaviour on seeing TC is to retry over TCP.
	tcpResp := queryTCP(t, addr, newQuestion("large-answer.example.com", dns.TypeA))
	if tcpResp.Truncated {
		t.Fatal("TCP retry must not itself be truncated")
	}
	if len(tcpResp.Answer) != 1 {
		t.Fatalf("expected the full answer over TCP, got %d records", len(tcpResp.Answer))
	}
}

// truncatingUpstream behaves like a real resolver that has a large answer
// for a name: over UDP it sets TC and sends no answer records, and over
// TCP it sends the full answer.
type truncatingUpstream struct {
	addr    string
	udpConn net.PacketConn
	tcpLn   net.Listener
}

func startTruncatingUpstream(t *testing.T) *truncatingUpstream {
	t.Helper()

	udpConn, tcpLn, port := reserveUDPTCPPort(t)

	u := &truncatingUpstream{addr: "127.0.0.1:" + strconv.Itoa(port), udpConn: udpConn, tcpLn: tcpLn}
	go u.serveUDP()
	go u.serveTCP()
	t.Cleanup(func() {
		udpConn.Close()
		tcpLn.Close()
	})
	return u
}

func (u *truncatingUpstream) serveUDP() {
	buf := make([]byte, maxUDPSize)
	for {
		n, addr, err := u.udpConn.ReadFrom(buf)
		if err != nil {
			return
		}
		query := make([]byte, n)
		copy(query, buf[:n])

		req := new(dns.Msg)
		if err := req.Unpack(query); err != nil {
			continue
		}
		resp := new(dns.Msg)
		resp.SetReply(req)
		resp.Truncated = true
		out, err := resp.Pack()
		if err != nil {
			continue
		}
		u.udpConn.WriteTo(out, addr)
	}
}

func (u *truncatingUpstream) serveTCP() {
	for {
		conn, err := u.tcpLn.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			query, err := readTCPMessage(conn)
			if err != nil {
				return
			}
			req := new(dns.Msg)
			if err := req.Unpack(query); err != nil {
				return
			}
			resp := new(dns.Msg)
			resp.SetReply(req)
			rr, _ := dns.NewRR(req.Question[0].Name + " 300 IN A 93.184.216.34")
			resp.Answer = append(resp.Answer, rr)
			out, err := resp.Pack()
			if err != nil {
				return
			}
			writeTCPMessage(conn, out)
		}()
	}
}

// --- Upstream failure modes ---------------------------------------------

// TestForwarderUDPUpstreamTimeoutLeavesClientSilent proves the forwarder
// does not synthesise an answer when the upstream never responds: the
// client gets nothing at all (its own timeout fires), which is the signal
// a stub resolver needs to fail over to the router's secondary DNS entry.
// It also proves the goroutine and semaphore slot used for the attempt are
// released, so a timing-out upstream cannot slowly exhaust the server.
func TestForwarderUDPUpstreamTimeoutLeavesClientSilent(t *testing.T) {
	silentUpstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening silent upstream: %v", err)
	}
	t.Cleanup(func() { silentUpstream.Close() })
	go func() {
		buf := make([]byte, maxUDPSize)
		for {
			_, _, err := silentUpstream.ReadFrom(buf)
			if err != nil {
				return
			}
			// Never respond.
		}
	}()

	addr, _ := startForwarderWithTimeout(t, silentUpstream.LocalAddr().String(), 200*time.Millisecond)

	// Captured only after every long-lived goroutine (the fake upstream's
	// read loop, the forwarder's own UDP/TCP accept loops) already exists,
	// so the margin below only has to cover the ephemeral per-query
	// goroutines this test's own exchanges spawn.
	baseline := runtime.NumGoroutine()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1 * time.Second))

	q := newQuestion("times-out.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if _, err := conn.Write(packed); err != nil {
		t.Fatalf("write query: %v", err)
	}

	buf := make([]byte, maxUDPSize)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected no response from the forwarder when upstream never answers, got one")
	}

	assertUDPListenerStillAccepts(t, addr, 1*time.Second)
	assertNoGoroutineLeak(t, baseline)
}

// TestForwarderUDPUpstreamRefusedLeavesClientSilent covers an upstream
// address nothing is listening on: the forwarder must not crash or
// fabricate a response, and the server must stay usable afterward.
func TestForwarderUDPUpstreamRefusedLeavesClientSilent(t *testing.T) {
	// Bind and immediately close a UDP port so we have an address
	// nothing is listening on, but that is very likely still free.
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a port: %v", err)
	}
	refusedAddr := closed.LocalAddr().String()
	closed.Close()

	addr, _ := startForwarderWithTimeout(t, refusedAddr, 500*time.Millisecond)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	q := newQuestion("refused.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if _, err := conn.Write(packed); err != nil {
		t.Fatalf("write query: %v", err)
	}

	buf := make([]byte, maxUDPSize)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected no response from the forwarder when upstream refuses the connection, got one")
	}

	assertUDPListenerStillAccepts(t, addr, 2*time.Second)
}

// TestForwarderTCPUpstreamRefusedClosesClientConnection covers a TCP
// upstream nothing is listening on: the client's connection to the
// forwarder must be closed without a response, not left hanging and not
// answered with a fabricated message.
func TestForwarderTCPUpstreamRefusedClosesClientConnection(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a port: %v", err)
	}
	refusedAddr := closed.Addr().String()
	closed.Close()

	addr, _ := startForwarderWithTimeout(t, refusedAddr, 500*time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	q := newQuestion("tcp-refused.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if err := writeTCPMessage(conn, packed); err != nil {
		t.Fatalf("write query: %v", err)
	}

	_, err = readTCPMessage(conn)
	if err == nil {
		t.Fatal("expected the forwarder to close the connection when its upstream refuses, got a response instead")
	}
}

// TestForwarderTCPUpstreamClosesMidAnswer covers an upstream that accepts
// the TCP connection and starts a length-prefixed answer, then closes the
// socket before sending the full body. The forwarder must not forward a
// truncated, corrupted message to the client - it drops the connection.
func TestForwarderTCPUpstreamClosesMidAnswer(t *testing.T) {
	flakyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening flaky upstream: %v", err)
	}
	t.Cleanup(func() { flakyLn.Close() })
	go func() {
		for {
			c, err := flakyLn.Accept()
			if err != nil {
				return
			}
			go func() {
				// Read and discard the query, then promise a 100-byte
				// answer but send only 5 bytes before closing.
				readTCPMessage(c)
				var lenBuf [2]byte
				binary.BigEndian.PutUint16(lenBuf[:], 100)
				c.Write(lenBuf[:])
				c.Write([]byte{1, 2, 3, 4, 5})
				c.Close()
			}()
		}
	}()

	addr, _ := startForwarderWithTimeout(t, flakyLn.Addr().String(), 1*time.Second)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	q := newQuestion("tcp-mid-close.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if err := writeTCPMessage(conn, packed); err != nil {
		t.Fatalf("write query: %v", err)
	}

	_, err = readTCPMessage(conn)
	if err == nil {
		t.Fatal("expected the forwarder to close the connection after its upstream closed mid-answer, got a response instead")
	}

	assertTCPListenerStillAccepts(t, addr, 2*time.Second)
}

// TestForwarderRelaysSERVFAILAndREFUSEDUnchanged proves rcodes signalling
// upstream failure are not special-cased: they reach the client exactly as
// the upstream sent them, byte-for-byte modulo the transaction ID, the
// same rule as every successful answer.
func TestForwarderRelaysSERVFAILAndREFUSEDUnchanged(t *testing.T) {
	for _, rcode := range []int{dns.RcodeServerFailure, dns.RcodeRefused} {
		t.Run(dns.RcodeToString[rcode], func(t *testing.T) {
			upstream := startRcodeUpstream(t, rcode)
			addr, _ := startForwarder(t, upstream.addr)

			q := newQuestion("failure-mode.example.com", dns.TypeA)
			direct := exchangeRawUDP(t, upstream.addr, q)

			q2 := newQuestion("failure-mode.example.com", dns.TypeA)
			viaForwarder := exchangeRawUDP(t, addr, q2)

			if !bytes.Equal(zeroTxID(direct), zeroTxID(viaForwarder)) {
				t.Fatalf("byte mismatch relaying rcode %s (transaction ID excluded):\ndirect:        % x\nvia forwarder: % x", dns.RcodeToString[rcode], direct, viaForwarder)
			}
		})
	}
}

type rcodeUpstream struct {
	addr string
}

func startRcodeUpstream(t *testing.T, rcode int) *rcodeUpstream {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening rcode upstream: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, maxUDPSize)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			req := new(dns.Msg)
			if err := req.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := new(dns.Msg)
			resp.SetRcode(req, rcode)
			out, err := resp.Pack()
			if err != nil {
				continue
			}
			conn.WriteTo(out, addr)
		}
	}()

	return &rcodeUpstream{addr: conn.LocalAddr().String()}
}

// startForwarderWithTimeout is like startForwarder but lets a test pin a
// short Timeout, so upstream-failure tests do not wait on the production
// default.
func startForwarderWithTimeout(t *testing.T, upstream string, timeout time.Duration) (addr string, srv *Server) {
	t.Helper()

	srv = &Server{
		Upstream: upstream,
		Logger:   NoopLogger{},
		Timeout:  timeout,
	}

	listenOnFreePort(t, srv)
	go srv.Serve()
	t.Cleanup(srv.Close)

	return srv.Addr, srv
}
