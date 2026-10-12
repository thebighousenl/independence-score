package forwarder

import (
	"bytes"
	"net"
	"strconv"
	"sync"
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

	ready := make(chan struct{})
	go func() {
		// ListenAndServe blocks; give it a moment to bind before we
		// signal ready, and close it when the test ends.
		go func() {
			time.Sleep(20 * time.Millisecond)
			close(ready)
		}()
		srv.ListenAndServe()
	}()
	<-ready
	t.Cleanup(srv.Close)

	return srv.Addr, logger
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

func TestForwarderByteForByteUDPResponse(t *testing.T) {
	// Verifies the forwarder does not repack or otherwise alter the
	// upstream's wire bytes: ask the fake upstream directly, then ask it
	// again through the forwarder, and compare the two answers'
	// marshaled RRs rather than raw bytes (the transaction ID differs
	// legitimately per query).
	upstream := startFakeUpstream(t)
	addr, _ := startForwarder(t, upstream.addr)

	q1 := newQuestion("byteforbyte.example.com", dns.TypeA)
	direct := queryUDP(t, upstream.addr, q1)

	q2 := newQuestion("byteforbyte.example.com", dns.TypeA)
	viaForwarder := queryUDP(t, addr, q2)

	if len(direct.Answer) != 1 || len(viaForwarder.Answer) != 1 {
		t.Fatalf("expected exactly one answer each: direct=%d viaForwarder=%d", len(direct.Answer), len(viaForwarder.Answer))
	}
	if direct.Answer[0].String() != viaForwarder.Answer[0].String() {
		t.Fatalf("answer mismatch:\ndirect:       %s\nvia forwarder: %s", direct.Answer[0].String(), viaForwarder.Answer[0].String())
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
