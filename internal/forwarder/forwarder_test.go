package forwarder

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeUpstream is a minimal "real" resolver for tests: it answers with a
// canned record appropriate to the question's qtype, over both UDP and
// TCP on the same port, so the test proves the forwarder relays an
// upstream's answer byte-for-byte rather than only exercising a mock.
type fakeUpstream struct {
	addr    string
	udpConn net.PacketConn
	tcpLn   net.Listener

	mu          sync.Mutex
	rawReceived [][]byte
}

func startFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening udp: %v", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port

	tcpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		udpConn.Close()
		t.Fatalf("listening tcp on same port %d: %v", port, err)
	}

	u := &fakeUpstream{
		addr:    "127.0.0.1:" + strconv.Itoa(port),
		udpConn: udpConn,
		tcpLn:   tcpLn,
	}

	go u.serveUDP()
	go u.serveTCP()

	t.Cleanup(func() {
		udpConn.Close()
		tcpLn.Close()
	})

	return u
}

func (u *fakeUpstream) serveUDP() {
	buf := make([]byte, maxUDPSize)
	for {
		n, addr, err := u.udpConn.ReadFrom(buf)
		if err != nil {
			return
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		u.recordRaw(query)
		resp := answerFor(query)
		if resp != nil {
			u.udpConn.WriteTo(resp, addr)
		}
	}
}

func (u *fakeUpstream) recordRaw(b []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rawReceived = append(u.rawReceived, b)
}

func (u *fakeUpstream) waitForRaw(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		if len(u.rawReceived) >= n {
			out := make([][]byte, len(u.rawReceived))
			copy(out, u.rawReceived)
			u.mu.Unlock()
			return out
		}
		u.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for upstream to receive %d raw datagrams", n)
	return nil
}

func (u *fakeUpstream) serveTCP() {
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
			resp := answerFor(query)
			if resp != nil {
				writeTCPMessage(conn, resp)
			}
		}()
	}
}

// answerFor builds a canned, correctly-typed answer for the single
// question in query, covering the record types M1 is scoped to pass
// through intact.
func answerFor(query []byte) []byte {
	req := new(dns.Msg)
	if err := req.Unpack(query); err != nil {
		return nil
	}
	if len(req.Question) == 0 {
		return nil
	}
	q := req.Question[0]

	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	switch q.Qtype {
	case dns.TypeA:
		rr, _ := dns.NewRR(q.Name + " 300 IN A 93.184.216.34")
		resp.Answer = append(resp.Answer, rr)
	case dns.TypeAAAA:
		rr, _ := dns.NewRR(q.Name + " 300 IN AAAA 2606:2800:220:1:248:1893:25c8:1946")
		resp.Answer = append(resp.Answer, rr)
	case dns.TypeCNAME:
		rr, _ := dns.NewRR(q.Name + " 300 IN CNAME canonical.example.")
		resp.Answer = append(resp.Answer, rr)
	case dns.TypeTXT:
		rr, _ := dns.NewRR(q.Name + ` 300 IN TXT "v=spf1 -all"`)
		resp.Answer = append(resp.Answer, rr)
	case dns.TypeSRV:
		rr, _ := dns.NewRR(q.Name + " 300 IN SRV 10 60 5060 target.example.")
		resp.Answer = append(resp.Answer, rr)
	case dns.TypePTR:
		rr, _ := dns.NewRR(q.Name + " 300 IN PTR host.example.")
		resp.Answer = append(resp.Answer, rr)
	case dns.TypeHTTPS:
		rr, _ := dns.NewRR(q.Name + " 300 IN HTTPS 1 . alpn=h2")
		resp.Answer = append(resp.Answer, rr)
	}

	// Mirror EDNS0, including a DNSSEC OK bit, and attach an RRSIG when
	// the client asked for DNSSEC data - the forwarder must relay these
	// opaque records unchanged rather than stripping them.
	if opt := req.IsEdns0(); opt != nil {
		resp.SetEdns0(opt.UDPSize(), opt.Do())
		if opt.Do() {
			rrsig, _ := dns.NewRR(q.Name + " 300 IN RRSIG A 8 2 300 20300101000000 20200101000000 12345 example. c2lnbmF0dXJl")
			resp.Answer = append(resp.Answer, rrsig)
		}
	}

	out, err := resp.Pack()
	if err != nil {
		return nil
	}
	return out
}

func startForwarder(t *testing.T, upstream string) (addr string, logger *recordingLogger) {
	t.Helper()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking forwarder port: %v", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	udpConn.Close()

	logger = &recordingLogger{}
	srv := &Server{
		Addr:     "127.0.0.1:" + strconv.Itoa(port),
		Upstream: upstream,
		Logger:   logger,
		Timeout:  2 * time.Second,
	}

	// Listen binds synchronously in this goroutine, so by the time it
	// returns every field Close touches is already written - Serve can
	// safely run in the background with Close callable at any point
	// after this, with no sleep-based guess required.
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(srv.Close)

	return srv.Addr, logger
}

// startForwarderWithCap is like startForwarder but lets a test pin
// MaxConcurrentUDP below its production default, so a concurrency test
// can exercise the ceiling without sending thousands of queries.
func startForwarderWithCap(t *testing.T, upstream string, maxConcurrentUDP int) (addr string, srv *Server) {
	t.Helper()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking forwarder port: %v", err)
	}
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	udpConn.Close()

	srv = &Server{
		Addr:             "127.0.0.1:" + strconv.Itoa(port),
		Upstream:         upstream,
		Logger:           NoopLogger{},
		Timeout:          5 * time.Second,
		MaxConcurrentUDP: maxConcurrentUDP,
	}

	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(srv.Close)

	return srv.Addr, srv
}

type recordingLogger struct {
	mu    sync.Mutex
	calls []loggedQuery
}

type loggedQuery struct {
	Hostname string
	Qtype    uint16
}

func (l *recordingLogger) LogQuery(hostname string, qtype uint16, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, loggedQuery{Hostname: hostname, Qtype: qtype})
}

func (l *recordingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func (l *recordingLogger) wait(t *testing.T, n int) []loggedQuery {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		if len(l.calls) >= n {
			out := make([]loggedQuery, len(l.calls))
			copy(out, l.calls)
			l.mu.Unlock()
			return out
		}
		l.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d logged queries", n)
	return nil
}

func queryUDP(t *testing.T, addr string, q *dns.Msg) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(q, addr)
	if err != nil {
		t.Fatalf("udp exchange: %v", err)
	}
	return resp
}

func queryTCP(t *testing.T, addr string, q *dns.Msg) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(q, addr)
	if err != nil {
		t.Fatalf("tcp exchange: %v", err)
	}
	return resp
}

func newQuestion(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	return m
}

type typeCase struct {
	name  string
	qtype uint16
}

var conformanceCases = []typeCase{
	{"A", dns.TypeA},
	{"AAAA", dns.TypeAAAA},
	{"CNAME", dns.TypeCNAME},
	{"TXT", dns.TypeTXT},
	{"SRV", dns.TypeSRV},
	{"PTR", dns.TypePTR},
	{"HTTPS", dns.TypeHTTPS},
}

func TestForwarderUDPPassesEachRecordTypeThroughIntact(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	for _, tc := range conformanceCases {
		t.Run(tc.name, func(t *testing.T) {
			q := newQuestion("example.com", tc.qtype)
			resp := queryUDP(t, addr, q)

			if resp.Rcode != dns.RcodeSuccess {
				t.Fatalf("rcode = %v, want success", resp.Rcode)
			}
			if len(resp.Answer) != 1 {
				t.Fatalf("got %d answers, want 1", len(resp.Answer))
			}
			if resp.Answer[0].Header().Rrtype != tc.qtype {
				t.Fatalf("answer type = %v, want %v", resp.Answer[0].Header().Rrtype, tc.qtype)
			}
		})
	}
}

func TestForwarderTCPPassesRecordThroughIntact(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	q := newQuestion("example.com", dns.TypeA)
	resp := queryTCP(t, addr, q)

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want success", resp.Rcode)
	}
	if len(resp.Answer) != 1 || resp.Answer[0].Header().Rrtype != dns.TypeA {
		t.Fatalf("unexpected TCP answer: %+v", resp.Answer)
	}
}

func TestForwarderPreservesEDNS0AndDNSSEC(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	q := newQuestion("example.com", dns.TypeA)
	q.SetEdns0(4096, true) // DO bit set: ask for DNSSEC data

	resp := queryUDP(t, addr, q)

	opt := resp.IsEdns0()
	if opt == nil {
		t.Fatal("expected EDNS0 OPT record to survive the round trip")
	}
	if !opt.Do() {
		t.Fatal("expected DNSSEC OK (DO) bit to survive the round trip")
	}

	var sawRRSIG bool
	for _, rr := range resp.Answer {
		if rr.Header().Rrtype == dns.TypeRRSIG {
			sawRRSIG = true
		}
	}
	if !sawRRSIG {
		t.Fatal("expected RRSIG record to be forwarded unchanged, not stripped")
	}
}

// exchangeRawUDP sends q's packed wire bytes to addr over UDP and returns
// the raw response bytes, with no parsing on the way out or back in. This
// is what makes the comparisons in TestForwarderUDPByteForByteAcrossAllTypes
// meaningful: dns.Client.Exchange decodes the response into a *dns.Msg and
// then re-encodes it on demand, which would hide a forwarder bug that
// corrupts bytes a round trip through the library happens to tolerate.
func exchangeRawUDP(t *testing.T, addr string, q *dns.Msg) []byte {
	t.Helper()
	packed, err := q.Pack()
	if err != nil {
		t.Fatalf("packing query: %v", err)
	}

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := conn.Write(packed); err != nil {
		t.Fatalf("write query: %v", err)
	}
	buf := make([]byte, maxUDPSize)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return buf[:n]
}

// zeroTxID clears the 2-byte DNS transaction ID so two otherwise-identical
// messages compare equal regardless of which random ID their query used.
// This is the only field TestForwarderUDPByteForByteAcrossAllTypes allows to
// differ: the client generates a fresh random ID per query, and the
// upstream's reply always mirrors whichever ID it was asked with, so the
// direct and via-forwarder exchanges are never going to share one.
func zeroTxID(msg []byte) []byte {
	out := make([]byte, len(msg))
	copy(out, msg)
	if len(out) >= 2 {
		out[0] = 0
		out[1] = 0
	}
	return out
}

// TestForwarderUDPByteForByteAcrossAllTypes makes byte-level equivalence
// the rule for every conformance-scoped record type, not a single sampled
// case: for each type, the exact bytes a client gets through the forwarder
// must equal the exact bytes it gets asking the upstream directly, modulo
// the transaction ID.
func TestForwarderUDPByteForByteAcrossAllTypes(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	for _, tc := range conformanceCases {
		t.Run(tc.name, func(t *testing.T) {
			name := "byteforbyte-" + tc.name + ".example.com"

			direct := exchangeRawUDP(t, upstream.addr, newQuestion(name, tc.qtype))
			viaForwarder := exchangeRawUDP(t, addr, newQuestion(name, tc.qtype))

			if !bytes.Equal(zeroTxID(direct), zeroTxID(viaForwarder)) {
				t.Fatalf("byte mismatch for %s (transaction ID excluded):\ndirect:        % x\nvia forwarder: % x", tc.name, direct, viaForwarder)
			}
		})
	}
}

// TestForwarderUDPByteForByteWithEDNS0DNSSEC extends the same byte-level
// rule to a query carrying EDNS(0) with the DNSSEC OK bit set, where the
// upstream's reply includes an RRSIG - the forwarder must relay the OPT
// pseudo-record and the signature unchanged, not just the plain answer.
func TestForwarderUDPByteForByteWithEDNS0DNSSEC(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	newEdnsQuestion := func(name string) *dns.Msg {
		q := newQuestion(name, dns.TypeA)
		q.SetEdns0(4096, true)
		return q
	}

	direct := exchangeRawUDP(t, upstream.addr, newEdnsQuestion("byteforbyte-edns0.example.com"))
	viaForwarder := exchangeRawUDP(t, addr, newEdnsQuestion("byteforbyte-edns0.example.com"))

	if !bytes.Equal(zeroTxID(direct), zeroTxID(viaForwarder)) {
		t.Fatalf("EDNS0/DNSSEC byte mismatch (transaction ID excluded):\ndirect:        % x\nvia forwarder: % x", direct, viaForwarder)
	}
}

// TestForwarderTCPByteForByte is the TCP-fallback analogue of the UDP
// byte-equivalence suite above: the length-prefixed message the client
// reads back over TCP must match the upstream's own TCP answer exactly,
// modulo the transaction ID.
func TestForwarderTCPByteForByte(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	exchangeRawTCP := func(t *testing.T, target string, q *dns.Msg) []byte {
		t.Helper()
		packed, err := q.Pack()
		if err != nil {
			t.Fatalf("packing query: %v", err)
		}
		conn, err := net.Dial("tcp", target)
		if err != nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := writeTCPMessage(conn, packed); err != nil {
			t.Fatalf("write query: %v", err)
		}
		resp, err := readTCPMessage(conn)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return resp
	}

	direct := exchangeRawTCP(t, upstream.addr, newQuestion("byteforbyte-tcp.example.com", dns.TypeA))
	viaForwarder := exchangeRawTCP(t, addr, newQuestion("byteforbyte-tcp.example.com", dns.TypeA))

	if !bytes.Equal(zeroTxID(direct), zeroTxID(viaForwarder)) {
		t.Fatalf("TCP byte mismatch (transaction ID excluded):\ndirect:        % x\nvia forwarder: % x", direct, viaForwarder)
	}
}

func TestForwarderLogsHostnameAfterRespondingWithoutAlteringAnswer(t *testing.T) {
	upstream := startFakeUpstream(t)
	addr, logger := startForwarder(t, upstream.addr)

	q := newQuestion("logged.example.com", dns.TypeA)
	resp := queryUDP(t, addr, q)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	calls := logger.wait(t, 1)
	if calls[0].Hostname != "logged.example.com." {
		t.Fatalf("logged hostname = %q, want %q", calls[0].Hostname, "logged.example.com.")
	}
	if calls[0].Qtype != dns.TypeA {
		t.Fatalf("logged qtype = %v, want A", calls[0].Qtype)
	}
}

func TestForwarderForwardsUnparseableQueryUnchanged(t *testing.T) {
	// A forward-only proxy must still forward bytes it cannot itself
	// parse, rather than rejecting or dropping them: send a datagram
	// that isn't a valid DNS message straight at the forwarder's UDP
	// port and confirm the upstream receives those exact bytes.
	upstream := startFakeUpstream(t)
	addr, logger := startForwarder(t, upstream.addr)

	garbage := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03}

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(garbage); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	received := upstream.waitForRaw(t, 1)
	if !bytes.Equal(received[0], garbage) {
		t.Fatalf("upstream received %x, want unchanged %x", received[0], garbage)
	}

	// Logging is best-effort on top of the already-forwarded bytes: an
	// unparseable query must not be logged, but it must not have
	// blocked or altered forwarding either (checked above).
	time.Sleep(50 * time.Millisecond)
	if n := logger.count(); n != 0 {
		t.Fatalf("expected 0 logged queries for unparseable input, got %d", n)
	}
}

// TestForwarderUDPConcurrencyIsBounded drives far more concurrent UDP
// queries than a pinned MaxConcurrentUDP ceiling, against an upstream that
// holds each request open briefly so true concurrency is exercised, and
// proves two things at once: the ceiling actually holds (goroutines and
// upstream sockets never exceed it), and the forward-only guarantee is not
// broken to enforce it - every single query still gets its upstream
// answer, none are dropped, rewritten, or answered locally.
func TestForwarderUDPConcurrencyIsBounded(t *testing.T) {
	const ceiling = 4
	const totalQueries = 40

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening fake upstream: %v", err)
	}
	t.Cleanup(func() { udpConn.Close() })

	var inFlight, maxInFlight, handled int32
	go func() {
		buf := make([]byte, maxUDPSize)
		for {
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			query := make([]byte, n)
			copy(query, buf[:n])
			go func() {
				cur := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&maxInFlight)
					if cur <= old {
						break
					}
					if atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
						break
					}
				}
				// Hold the request open long enough that, if the
				// ceiling did not hold, many more than `ceiling`
				// requests would overlap in flight.
				time.Sleep(20 * time.Millisecond)
				resp := answerFor(query)
				// Decrement before writing the response, not after:
				// once the response is on the wire the forwarder can
				// complete its round trip, free its semaphore slot,
				// and admit the next query - all before this goroutine
				// gets scheduled again to run its own decrement. That
				// ordering let an admission the ceiling genuinely
				// bounded look, from this side, like a transient
				// overshoot.
				atomic.AddInt32(&inFlight, -1)
				atomic.AddInt32(&handled, 1)
				if resp != nil {
					udpConn.WriteTo(resp, addr)
				}
			}()
		}
	}()

	addr, _ := startForwarderWithCap(t, udpConn.LocalAddr().String(), ceiling)

	var wg sync.WaitGroup
	var successes int32
	for i := 0; i < totalQueries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := newQuestion(fmt.Sprintf("flood-%d.example.com", i), dns.TypeA)
			c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
			resp, _, err := c.Exchange(q, addr)
			if err != nil {
				t.Errorf("query %d: exchange failed: %v", i, err)
				return
			}
			if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
				t.Errorf("query %d: unexpected response: %+v", i, resp)
				return
			}
			atomic.AddInt32(&successes, 1)
		}(i)
	}
	wg.Wait()

	if got := int(successes); got != totalQueries {
		t.Fatalf("got %d successful answers, want all %d queries answered", got, totalQueries)
	}
	if got := atomic.LoadInt32(&maxInFlight); got > int32(ceiling) {
		t.Fatalf("observed %d concurrent upstream requests, want <= %d (the configured ceiling)", got, ceiling)
	}
	t.Logf("ceiling=%d max-observed-concurrency=%d queries-handled=%d", ceiling, atomic.LoadInt32(&maxInFlight), atomic.LoadInt32(&handled))
}

// TestForwarderUDPDoesNotRelayStaleResponseFromReusedSocket guards the
// correctness cost of pooling upstream UDP sockets: once a socket is
// reused for a second query, a stale duplicate answer to the *first*
// query can still be sitting in its receive buffer. Pinning
// MaxConcurrentUDP to 1 forces every query through the same pooled
// socket, so this reproduces that ordering deterministically. The fake
// upstream deliberately resends query A's answer a second time, timed to
// land after query A has already been answered but before query B's
// request reaches it - exactly the race that would let a reused socket
// serve the wrong client's answer if responses were not matched against
// the query's own transaction ID.
func TestForwarderUDPDoesNotRelayStaleResponseFromReusedSocket(t *testing.T) {
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening fake upstream: %v", err)
	}
	t.Cleanup(func() { udpConn.Close() })

	go func() {
		buf := make([]byte, maxUDPSize)
		for {
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			query := make([]byte, n)
			copy(query, buf[:n])
			resp := answerFor(query)
			if resp == nil {
				continue
			}
			udpConn.WriteTo(resp, addr)
			if bytes.Contains(query, []byte("stale-trigger")) {
				// Resend the same answer again shortly after: a
				// duplicate that lands in the pooled socket's
				// receive buffer only after this round trip has
				// already completed and the socket has been
				// returned to the pool.
				go func() {
					time.Sleep(20 * time.Millisecond)
					udpConn.WriteTo(resp, addr)
				}()
			}
		}
	}()

	addr, _ := startForwarderWithCap(t, udpConn.LocalAddr().String(), 1)

	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}

	qA := newQuestion("stale-trigger.example.com", dns.TypeA)
	respA, _, err := c.Exchange(qA, addr)
	if err != nil {
		t.Fatalf("query A: exchange failed: %v", err)
	}
	if len(respA.Answer) != 1 || respA.Answer[0].Header().Name != "stale-trigger.example.com." {
		t.Fatalf("query A: unexpected response: %+v", respA)
	}

	// Give the fake upstream's delayed duplicate time to land in the
	// (now pooled, idle) socket's receive buffer before query B reuses
	// it.
	time.Sleep(40 * time.Millisecond)

	qB := newQuestion("real-query-b.example.com", dns.TypeAAAA)
	respB, _, err := c.Exchange(qB, addr)
	if err != nil {
		t.Fatalf("query B: exchange failed: %v", err)
	}
	if len(respB.Answer) != 1 {
		t.Fatalf("query B: unexpected response: %+v", respB)
	}
	if got := respB.Answer[0].Header().Name; got != "real-query-b.example.com." {
		t.Fatalf("query B got an answer for %q - the stale duplicate for query A was relayed to the wrong query", got)
	}
	if respB.Answer[0].Header().Rrtype != dns.TypeAAAA {
		t.Fatalf("query B: expected an AAAA answer matching its own question, got rrtype %d (stale A-record answer leaked across queries)", respB.Answer[0].Header().Rrtype)
	}
}
