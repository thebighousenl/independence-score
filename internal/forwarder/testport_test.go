package forwarder

import (
	"net"
	"strconv"
	"testing"
)

// portAttempts is how many candidate ports a helper below tries before it
// gives up. One unlucky draw happens often enough to fail a CI run; several
// unlucky draws in a row does not happen on an idle loopback interface.
const portAttempts = 20

// reserveUDPTCPPort returns a loopback port held open for UDP and TCP at the
// same time - what a DNS listener needs, since DNS uses the same port number
// for both.
//
// The kernel keeps separate port spaces for UDP and TCP, so a port handed out
// by "udp 127.0.0.1:0" can already be in use for TCP, by another test in this
// package or by anything else on the machine. Asking for the pair in a single
// draw therefore fails now and then with "bind: address already in use" - and
// under the race detector the tests run slower and hold their sockets longer,
// so the failure is more likely there. Drawing again removes the flake.
//
// The caller owns both sockets and must close them.
func reserveUDPTCPPort(t *testing.T) (net.PacketConn, net.Listener, int) {
	t.Helper()

	var lastErr error
	for i := 0; i < portAttempts; i++ {
		udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			lastErr = err
			continue
		}
		port := udpConn.LocalAddr().(*net.UDPAddr).Port

		tcpLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			udpConn.Close()
			lastErr = err
			continue
		}
		return udpConn, tcpLn, port
	}

	t.Fatalf("no loopback port free for both udp and tcp after %d attempts: %v", portAttempts, lastErr)
	return nil, nil, 0
}

// listenOnFreePort points srv at a free loopback port and binds it, retrying
// on a lost race for the port.
//
// Server.Listen binds the sockets itself, so a test cannot hand it a socket -
// only a port number, named first and bound a moment later. Anything on the
// machine can take that port inside the gap. A test that fails there has
// proved nothing about the forwarder, so draw another port and try again.
func listenOnFreePort(t *testing.T, srv *Server) {
	t.Helper()

	var lastErr error
	for i := 0; i < portAttempts; i++ {
		udpConn, tcpLn, port := reserveUDPTCPPort(t)
		// Let the pair go only once both halves are known free, which keeps
		// the window where srv can lose the port as short as possible.
		udpConn.Close()
		tcpLn.Close()

		srv.Addr = "127.0.0.1:" + strconv.Itoa(port)
		if err := srv.Listen(); err != nil {
			lastErr = err
			continue
		}
		return
	}

	t.Fatalf("listen after %d port attempts: %v", portAttempts, lastErr)
}
