// The brief refuses high availability for this forwarder: a user's router
// is expected to carry a secondary DNS entry, and the network survives on
// that secondary resolver whenever this container is down. That only works
// if a stub resolver's normal failover trigger - getting no answer at all -
// actually fires. These tests prove the two situations a user needs to be
// able to tell apart (see the README's Failover section): the container
// itself being down, and the container being up but unable to reach its
// configured upstream. In both, the client must see silence or a closed
// connection, never a synthesised SERVFAIL or an empty NOERROR - either of
// those is an answer, and an answer is exactly what stops a stub resolver
// from trying its secondary entry.
package forwarder

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// TestForwarderDownLeavesClientWithNoAnswer covers "the container itself
// is down": nothing is listening on the forwarder's address at all, the
// same as a stopped or crashed container. A client must see the ordinary
// absence of a listener (a UDP read that never completes, a TCP dial that
// is refused) - not any kind of DNS response.
func TestForwarderDownLeavesClientWithNoAnswer(t *testing.T) {
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a port: %v", err)
	}
	addr := udpConn.LocalAddr().String()
	udpConn.Close() // nothing is listening from here on - "container is down"

	t.Run("UDP", func(t *testing.T) {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(1 * time.Second))

		q := newQuestion("container-down.example.com", dns.TypeA)
		packed, _ := q.Pack()
		if _, err := conn.Write(packed); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, maxUDPSize)
		if _, err := conn.Read(buf); err == nil {
			t.Fatal("expected no answer from a down forwarder, got one")
		}
	})

	t.Run("TCP", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			conn.Close()
			t.Fatal("expected the TCP dial itself to fail against a down forwarder, it succeeded")
		}
	})
}

// TestForwarderUpstreamUnreachableNeverSynthesizesAnswer covers "the
// container is up but cannot reach its configured upstream": the opposite
// failure mode from the container being down, and the one most easily
// confused with it from a dashboard or log line alone (see the README).
// What must hold here is the same rule as the down-container case even
// though the forwarder process is alive and its listeners are open: the
// client still gets nothing it could mistake for an authoritative answer.
func TestForwarderUpstreamUnreachableNeverSynthesizesAnswer(t *testing.T) {
	// An address nothing is listening on, so every forward attempt fails.
	deadUpstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a port: %v", err)
	}
	upstreamAddr := deadUpstream.LocalAddr().String()
	deadUpstream.Close()

	addr, _ := startForwarderWithTimeout(t, upstreamAddr, 500*time.Millisecond)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	q := newQuestion("upstream-unreachable.example.com", dns.TypeA)
	packed, _ := q.Pack()
	if _, err := conn.Write(packed); err != nil {
		t.Fatalf("write query: %v", err)
	}

	buf := make([]byte, maxUDPSize)
	n, err := conn.Read(buf)
	if err == nil {
		// Got *something* back - prove it is not a fabricated DNS
		// message. Any real response here would be a bug: the only
		// correct outcome is that this read errors out on the client's
		// own deadline, the same silence a down container produces.
		t.Fatalf("forwarder answered with %d bytes while its upstream was unreachable, want no answer at all (got: % x)", n, buf[:n])
	}
}
