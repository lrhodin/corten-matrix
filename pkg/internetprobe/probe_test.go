package internetprobe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestOutcomeOrderingRanksStrongestVerdictLast(t *testing.T) {
	// queryProvider picks the strongest verdict across addresses with a numeric
	// max, so this ordering is load-bearing: an address that actually reached
	// the network must outrank one we were never allowed to try.
	if !(OutcomeBlocked < OutcomeUnreachable && OutcomeUnreachable < OutcomeReachable) {
		t.Fatal("Outcome ordering must be Blocked < Unreachable < Reachable")
	}
	if Outcome(0) != OutcomeBlocked {
		t.Fatal("the zero Outcome must be Blocked so an unpopulated Result never reads as an outage")
	}
}

func TestZeroResultIsUnknownNotAnOutage(t *testing.T) {
	var result Result
	if result.Reachable() {
		t.Fatal("zero Result reports reachable")
	}
	if !result.Blocked() {
		t.Fatal("zero Result must report Blocked (unknown), not a confirmed outage")
	}
}

func TestEveryProviderIsProbedOverBothAddressFamilies(t *testing.T) {
	// An IPv6-only or NAT64 host has no IPv4 route at all, so probing only IPv4
	// literals would report a permanent outage on a healthy bridge.
	for name, addresses := range map[string][]string{
		"cloudflare": cloudflareAddresses,
		"google":     googleAddresses,
	} {
		if len(addresses) != 2 {
			t.Fatalf("%s: want one IPv4 and one IPv6 literal, got %v", name, addresses)
		}
		v4, v6 := net.ParseIP(addresses[0]), net.ParseIP(addresses[1])
		if v4 == nil || v4.To4() == nil {
			t.Errorf("%s: %q is not an IPv4 literal", name, addresses[0])
		}
		if v6 == nil || v6.To4() != nil {
			t.Errorf("%s: %q is not an IPv6 literal", name, addresses[1])
		}
	}

	// And over every leg: TLS on 443 first (the primary proof), TLS on 853 as
	// the authenticated fallback, and plaintext DNS on 53 as a diagnostic that
	// can never vote Reachable.
	legs := probeLegs()
	if len(legs) != 3 ||
		legs[0].name != "tls" || legs[0].port != "443" || !legs[0].voting ||
		legs[1].name != "dot" || legs[1].port != "853" || !legs[1].voting ||
		legs[2].name != "dns" || legs[2].port != "53" || legs[2].voting {
		t.Fatalf("probe legs = %+v, want voting tls:443 and dot:853 then diagnostic dns:53 (port 80 is deliberately not a leg, and 53 must never vote)", legs)
	}

	// The race must actually dial every (address, method) pair of a provider.
	dialed := recordDials(t, syscall.ENETUNREACH)
	if outcome, _ := queryProvider(context.Background(), cloudflareAddresses); outcome != OutcomeUnreachable {
		t.Fatalf("outcome = %s, want unreachable when every leg has no route", outcome)
	}
	want := map[string]bool{}
	for _, address := range cloudflareAddresses {
		for _, leg := range legs {
			want[net.JoinHostPort(address, leg.port)] = true
		}
	}
	got := dialed()
	if len(got) != len(want) {
		t.Fatalf("dialed %v, want every address over every method: %v", got, want)
	}
	for address := range want {
		if !got[address] {
			t.Errorf("leg %s was never dialed", address)
		}
	}
}

// recordDials replaces the dial seam with one that fails every dial with errno
// and returns a snapshot function of the addresses dialed.
func recordDials(t *testing.T, errno syscall.Errno) func() map[string]bool {
	t.Helper()
	saved := dialContext
	t.Cleanup(func() { dialContext = saved })
	var mu sync.Mutex
	dialed := map[string]bool{}
	dialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		dialed[address] = true
		mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errno}
	}
	return func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]bool{}
		for k, v := range dialed {
			out[k] = v
		}
		return out
	}
}

// fixtureCert builds a self-signed certificate for the given IP SANs, as both a
// serving certificate and a root pool that trusts it.
func fixtureCert(t *testing.T, ips ...net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	return fixtureCertValid(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), ips...)
}

func fixtureCertValid(t *testing.T, notBefore, notAfter time.Time, ips ...net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "internetprobe fixture"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pool
}

// startFixtureTLS serves TLS handshakes with cert on a loopback port.
func startFixtureTLS(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.(*tls.Conn).Handshake()
			}()
		}
	}()
	return listener.Addr().String()
}

// closedPort returns a loopback address nothing listens on (ECONNREFUSED).
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

// useLegs points the three legs at fixture addresses (all on 127.0.0.1) and
// installs roots as the TLS trust store; nil roots means the system roots and
// an empty pool means a host with no CA bundle at all.
func useLegs(t *testing.T, tlsAddr, dotAddr, dnsAddr string, roots *x509.CertPool) {
	t.Helper()
	savedTLS, savedDoT, savedDNS, savedRoots := tlsPort, dotPort, dnsPort, tlsRootCAs
	t.Cleanup(func() { tlsPort, dotPort, dnsPort, tlsRootCAs = savedTLS, savedDoT, savedDNS, savedRoots })
	_, tlsPort, _ = net.SplitHostPort(tlsAddr)
	_, dotPort, _ = net.SplitHostPort(dotAddr)
	_, dnsPort, _ = net.SplitHostPort(dnsAddr)
	tlsRootCAs = roots
}

// startHijacker accepts on a port and answers with HTTP, like a captive portal
// intercepting whatever port it is put on.
func startHijacker(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = conn.Write([]byte("HTTP/1.1 302 Found\r\nLocation: http://portal/\r\n\r\n"))
			}()
		}
	}()
	return listener.Addr().String()
}

// The voting proof: only a certificate that chains to a trusted root AND names
// the dialed IP counts. A peer that is provably not the provider is
// unreachable; a failure that may lie with this host abstains, and the trust
// store's contents are never consulted to decide which.
func TestTLSLegAcceptsOnlyACertificateThatValidatesForTheDialedIP(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	server := startFixtureTLS(t, cert)
	_, unrelated := fixtureCert(t, loopback) // a non-empty trust store that does not contain the server's certificate
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leg := func(t *testing.T, roots *x509.CertPool, address string) (legVerdict, error) {
		t.Helper()
		useLegs(t, address, address, address, roots)
		return tlsOnce(ctx, address)
	}
	expect := func(t *testing.T, verdict legVerdict, err error, want legVerdict, wantErr error, why string) {
		t.Helper()
		if verdict != want || !errors.Is(err, wantErr) {
			t.Fatalf("verdict = %s, err = %v; want %s carrying %v — %s", verdict, err, want, wantErr, why)
		}
	}

	t.Run("verified IP SAN is reachable", func(t *testing.T) {
		verdict, err := leg(t, pool, server)
		if verdict != legReachable {
			t.Fatalf("verdict = %s (err %v), want reachable for a certificate that validates for the dialed IP", verdict, err)
		}
	})
	t.Run("an untrusted chain abstains even with a non-empty trust store", func(t *testing.T) {
		verdict, err := leg(t, unrelated, server)
		expect(t, verdict, err, legAbstain, errTLSUnverified, "a self-signed portal and a stale or malformed local bundle raise the same error; the pool's contents prove nothing")
	})
	t.Run("no CA bundle at all abstains", func(t *testing.T) {
		verdict, err := leg(t, x509.NewCertPool(), server)
		expect(t, verdict, err, legAbstain, errTLSUnverified, "an empty trust store proves nothing about the peer")
	})
	t.Run("a trusted certificate for a different IP is interception", func(t *testing.T) {
		otherCert, otherPool := fixtureCert(t, net.ParseIP("203.0.113.1"))
		otherServer := startFixtureTLS(t, otherCert)
		verdict, err := leg(t, otherPool, otherServer)
		expect(t, verdict, err, legUnreachable, errTLSRejected, "a portal with a real certificate for its own name is not the provider")
	})
	t.Run("a peer that does not speak TLS is interception", func(t *testing.T) {
		verdict, err := leg(t, pool, startHijacker(t))
		expect(t, verdict, err, legUnreachable, errTLSRejected, "the providers always speak TLS on these ports")
	})
	t.Run("an expired certificate abstains", func(t *testing.T) {
		expiredCert, expiredPool := fixtureCertValid(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), loopback)
		expiredServer := startFixtureTLS(t, expiredCert)
		verdict, err := leg(t, expiredPool, expiredServer)
		expect(t, verdict, err, legAbstain, errTLSUnverified, "clock skew on this host is indistinguishable from an expired portal certificate")
	})
}

// The classifier's table, row by row, including the default: an error nobody
// enumerated must abstain, never vote. SystemRootsError is the case that
// showed a permissive default manufactures outages from local failures.
func TestHandshakeClassifierVotesOnlyOnPositiveEvidenceOfInterception(t *testing.T) {
	timeout := &net.OpError{Op: "read", Net: "tcp", Err: &timeoutError{}}
	for _, tc := range []struct {
		name string
		err  error
		want legVerdict
	}{
		{"chain validates for another host", x509.HostnameError{Host: "127.0.0.1"}, legUnreachable},
		{"wrapped by tls.CertificateVerificationError", &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "127.0.0.1"}}, legUnreachable},
		{"peer does not speak TLS", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, legUnreachable},
		{"peer sent a fatal alert", tls.AlertError(40), legUnreachable},
		{"peer closed mid-handshake", io.ErrUnexpectedEOF, legUnreachable},
		{"connection reset mid-handshake", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, legUnreachable},
		{"untrusted chain", x509.UnknownAuthorityError{}, legAbstain},
		{"untrusted chain wrapped", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, legAbstain},
		{"system roots could not be loaded", x509.SystemRootsError{Err: errors.New("no such file")}, legAbstain},
		{"expired certificate", x509.CertificateInvalidError{Reason: x509.Expired}, legAbstain},
		{"any other certificate invalidity", x509.CertificateInvalidError{Reason: x509.IncompatibleUsage}, legAbstain},
		{"insecure signature algorithm", x509.InsecureAlgorithmError(0), legAbstain},
		{"our own cancellation", context.Canceled, legAbstain},
		{"our own deadline", context.DeadlineExceeded, legAbstain},
		{"i/o timeout after the connect", timeout, legAbstain},
		{"an error nobody enumerated", errors.New("tls: something new"), legAbstain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := classifyHandshakeError(tc.err)
			if verdict != tc.want {
				t.Fatalf("classify(%v) = %s, want %s", tc.err, verdict, tc.want)
			}
			wantErr := errTLSUnverified
			if tc.want == legUnreachable {
				wantErr = errTLSRejected
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("classify(%v) err = %v, want it to carry %v", tc.err, err, wantErr)
			}
		})
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

// The provider verdict across all three legs, in the cases that matter
// operationally. The diagnostic 53 leg contributes no verdict in either
// direction.
func TestProviderVerdictCombinesTheLegs(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	tlsServer := startFixtureTLS(t, cert)
	resolver := startFakeResolver(t)
	refused := closedPort(t)
	hijacker := startHijacker(t)
	host := []string{"127.0.0.1"}

	for _, tc := range []struct {
		name                      string
		tlsAddr, dotAddr, dnsAddr string
		roots                     *x509.CertPool
		want                      Outcome
		wantErr                   error
	}{
		{"a verified 443 alone is reachable", tlsServer, refused, refused, pool, OutcomeReachable, nil},
		{"443 refused but a verified 853 is reachable", refused, tlsServer, refused, pool, OutcomeReachable, nil},
		{"both TLS legs refused and 53 answering is NOT reachable", refused, refused, resolver, pool, OutcomeUnreachable, errDiagnosticAnswered},
		{"no CA bundle with 53 answering is blocked, not reachable", tlsServer, tlsServer, resolver, x509.NewCertPool(), OutcomeBlocked, errTLSUnverified},
		{"abstaining TLS legs with 53 refused is blocked: a diagnostic leg cannot vote down either", tlsServer, tlsServer, refused, x509.NewCertPool(), OutcomeBlocked, syscall.ECONNREFUSED},
		{"abstaining TLS legs with 53 hijacked is blocked for the same reason", tlsServer, tlsServer, hijacker, x509.NewCertPool(), OutcomeBlocked, errHijacked},
		{"a portal speaking HTTP on the TLS ports is unreachable", hijacker, hijacker, resolver, pool, OutcomeUnreachable, errTLSRejected},
		{"everything refused is unreachable", refused, refused, refused, nil, OutcomeUnreachable, syscall.ECONNREFUSED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useLegs(t, tc.tlsAddr, tc.dotAddr, tc.dnsAddr, tc.roots)
			outcome, err := queryProvider(context.Background(), host)
			if outcome != tc.want {
				t.Fatalf("outcome = %s (err %v), want %s", outcome, err, tc.want)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want it to carry %v so the log explains the legs", err, tc.wantErr)
			}
		})
	}
}

// Only dial-level failures classify, identically for every voting leg: a
// socket the host denied abstains, a family with no route is not evidence
// about the Internet, and anything else the network did is unreachable.
func TestDialLevelErrorsDecideTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		want  Outcome
	}{
		{"ENETUNREACH on every leg of every family is unreachable: the network is dead", syscall.ENETUNREACH, OutcomeUnreachable},
		{"EHOSTUNREACH on every leg of every family is unreachable", syscall.EHOSTUNREACH, OutcomeUnreachable},
		{"ECONNREFUSED on every leg is unreachable", syscall.ECONNREFUSED, OutcomeUnreachable},
		{"EACCES on every leg is blocked", syscall.EACCES, OutcomeBlocked},
		{"EMFILE on every leg is blocked", syscall.EMFILE, OutcomeBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = recordDials(t, tc.errno)
			outcome, err := queryProvider(context.Background(), cloudflareAddresses)
			if outcome != tc.want || !errors.Is(err, tc.errno) {
				t.Fatalf("outcome = %s, err = %v; want %s carrying %v", outcome, err, tc.want, tc.errno)
			}
		})
	}

	// Per-family shapes, driven through the dial seam with the real IPv4 and
	// IPv6 literals of a provider.
	byFamily := func(t *testing.T, v4, v6 func(address string) (net.Conn, error)) {
		t.Helper()
		saved := dialContext
		t.Cleanup(func() { dialContext = saved })
		dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(address)
			if net.ParseIP(host).To4() != nil {
				return v4(address)
			}
			return v6(address)
		}
	}
	fail := func(errno syscall.Errno) func(string) (net.Conn, error) {
		return func(string) (net.Conn, error) { return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errno} }
	}
	t.Run("a v6 family with no route cannot outvote abstaining v4 legs", func(t *testing.T) {
		byFamily(t, fail(syscall.EACCES), fail(syscall.ENETUNREACH))
		if outcome, err := queryProvider(context.Background(), cloudflareAddresses); outcome != OutcomeBlocked {
			t.Fatalf("outcome = %s (err %v), want blocked: a missing v6 route is not evidence about the Internet", outcome, err)
		}
	})
	t.Run("a dead network on a v4-only host is unreachable", func(t *testing.T) {
		byFamily(t, fail(syscall.ECONNREFUSED), fail(syscall.ENETUNREACH))
		if outcome, _ := queryProvider(context.Background(), cloudflareAddresses); outcome != OutcomeUnreachable {
			t.Fatalf("outcome = %s, want unreachable: v4 reached the network and found nothing", outcome)
		}
	})
	t.Run("a v4-only host with a working 443 is reachable", func(t *testing.T) {
		loopback := net.ParseIP("127.0.0.1")
		cert, pool := fixtureCert(t, loopback)
		tlsServer := startFixtureTLS(t, cert)
		useLegs(t, tlsServer, closedPort(t), closedPort(t), pool)
		real := dialContext
		byFamily(t,
			func(address string) (net.Conn, error) {
				// Route the provider's v4 literal to the loopback fixture on the
				// same port; the certificate is checked against 127.0.0.1, so
				// the leg must dial the fixture by its own address.
				_, port, _ := net.SplitHostPort(address)
				return real(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port))
			},
			fail(syscall.ENETUNREACH))
		// tlsOnce validates ServerName = the dialed literal (the provider's
		// v4), which the fixture certificate does not carry, so run the
		// verified leg directly against the fixture and the family fold
		// through the seam.
		if verdict, err := tlsOnce(context.Background(), tlsServer); verdict != legReachable {
			t.Fatalf("fixture leg = %s (%v), want reachable", verdict, err)
		}
		if outcome, _ := queryProvider(context.Background(), []string{"127.0.0.1", cloudflareTarget6}); outcome != OutcomeReachable {
			t.Fatalf("outcome = %s, want reachable: the v4 family proved the provider and the v6 no-route is irrelevant", outcome)
		}
	})
}

// A verified leg must not leave the provider race waiting on a leg that is
// already connected and blocked in a read, and a caller canceling the probe
// (bridge shutdown) must get its answer promptly too.
func TestCancellationReachesAConnectedSocket(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	tlsServer := startFixtureTLS(t, cert)
	silent := startSilentListener(t)

	t.Run("a winning leg cancels a connected but silent leg", func(t *testing.T) {
		useLegs(t, tlsServer, silent, silent, pool)
		start := time.Now()
		outcome, err := queryProvider(context.Background(), []string{"127.0.0.1"})
		if outcome != OutcomeReachable {
			t.Fatalf("outcome = %s (err %v), want reachable", outcome, err)
		}
		if elapsed := time.Since(start); elapsed > probeTimeout/2 {
			t.Fatalf("provider race took %v after the verified leg won: cancellation did not reach the connected socket", elapsed)
		}
	})
	t.Run("a caller canceling the probe returns promptly", func(t *testing.T) {
		useLegs(t, silent, silent, silent, pool)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		outcome, _ := queryProvider(ctx, []string{"127.0.0.1"})
		if outcome != OutcomeBlocked {
			t.Fatalf("outcome = %s, want blocked: a caller canceling us is not a verdict", outcome)
		}
		if elapsed := time.Since(start); elapsed > probeTimeout/2 {
			t.Fatalf("canceled probe took %v: bridge shutdown would wait out the whole budget", elapsed)
		}
	})
}

// startSilentListener accepts connections and never sends or closes them.
func startSilentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	return listener.Addr().String()
}

// A host that silently drops a port must not make every probe take the whole
// probeTimeout once a voting leg has already proved the provider.
func TestAWinningLegCancelsTheRestOfTheProviderRace(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	tlsServer := startFixtureTLS(t, cert)
	// Distinct 853 and 53 addresses, so the seam below can blackhole 53 alone.
	useLegs(t, tlsServer, closedPort(t), closedPort(t), pool)

	saved := dialContext
	t.Cleanup(func() { dialContext = saved })
	dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(address)
		if port == dnsPort {
			<-ctx.Done() // a blackholed 53: nothing ever comes back
			return nil, ctx.Err()
		}
		return saved(ctx, network, address)
	}

	start := time.Now()
	outcome, err := queryProvider(context.Background(), []string{"127.0.0.1"})
	elapsed := time.Since(start)
	if outcome != OutcomeReachable {
		t.Fatalf("outcome = %s (err %v), want reachable from the verified 443 leg", outcome, err)
	}
	if elapsed > probeTimeout/2 {
		t.Fatalf("provider race took %v: the verified leg must cancel the leg that is still timing out", elapsed)
	}
}

func TestProbeWithReportsEachProvider(t *testing.T) {
	googleErr := errors.New("google probe failed")
	result := ProbeWith(context.Background(), func(_ context.Context, addresses []string) (Outcome, error) {
		if addresses[0] == CloudflareTarget {
			return OutcomeReachable, nil
		}
		return OutcomeUnreachable, googleErr
	})
	if result.Cloudflare != OutcomeReachable {
		t.Fatalf("Cloudflare outcome = %s, want reachable", result.Cloudflare)
	}
	if result.Google != OutcomeUnreachable {
		t.Fatalf("Google outcome = %s, want unreachable", result.Google)
	}
	if !errors.Is(result.GoogleErr, googleErr) {
		t.Fatalf("Google error = %v, want the runner's error retained for logging", result.GoogleErr)
	}
	if !result.Reachable() || result.Blocked() {
		t.Fatal("one answered provider must establish reachability and clear Blocked")
	}
}

func TestProbeWithProbesBothProvidersAndAcceptsEither(t *testing.T) {
	var mu sync.Mutex
	called := map[string]int{}
	runner := func(_ context.Context, addresses []string) (Outcome, error) {
		mu.Lock()
		called[addresses[0]]++
		mu.Unlock()
		if addresses[0] == GoogleTarget {
			return OutcomeReachable, nil
		}
		return OutcomeUnreachable, errors.New("no route")
	}

	if !ProbeWith(context.Background(), runner).Reachable() {
		t.Fatal("one answered provider should establish reachability")
	}
	if called[CloudflareTarget] != 1 || called[GoogleTarget] != 1 {
		t.Fatalf("probe calls = %#v, want one call to each provider", called)
	}
}

func TestProbeWithReportsBlockedOnlyWhenNoProviderTestedTheNetwork(t *testing.T) {
	bothBlocked := ProbeWith(context.Background(), func(context.Context, []string) (Outcome, error) {
		return OutcomeBlocked, syscall.EACCES
	})
	if !bothBlocked.Blocked() {
		t.Fatal("a probe denied on every provider must report Blocked so recovery treats it as unknown")
	}

	mixed := ProbeWith(context.Background(), func(_ context.Context, addresses []string) (Outcome, error) {
		if addresses[0] == CloudflareTarget {
			return OutcomeBlocked, syscall.EPERM
		}
		return OutcomeUnreachable, errors.New("network is unreachable")
	})
	if mixed.Blocked() {
		t.Fatal("Blocked must require that NO provider tested the network")
	}
	if mixed.Reachable() {
		t.Fatal("mixed blocked/unreachable is not reachable")
	}
}

// startFakeResolver answers DNS-over-TCP the way a real resolver does.
func startFakeResolver(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var prefix [2]byte
				if _, err := readFull(conn, prefix[:]); err != nil {
					return
				}
				query := make([]byte, binary.BigEndian.Uint16(prefix[:]))
				if _, err := readFull(conn, query); err != nil {
					return
				}
				reply := make([]byte, 0, 14)
				reply = binary.BigEndian.AppendUint16(reply, 12)
				reply = append(reply, query[0], query[1]) // echo the id
				reply = append(reply, 0x81, 0x80)         // QR + RD + RA
				reply = binary.BigEndian.AppendUint16(reply, 1)
				reply = binary.BigEndian.AppendUint16(reply, 0)
				reply = binary.BigEndian.AppendUint16(reply, 0)
				reply = binary.BigEndian.AppendUint16(reply, 0)
				_, _ = conn.Write(reply)
			}()
		}
	}()
	return listener.Addr().String()
}

// TestQueryOnceNeedsNoElevatedPrivileges is the portability guarantee that
// replacing ICMP was for: an ordinary TCP socket works as the service account,
// with no CAP_NET_RAW and no ping_group_range tuning.
func TestQueryOnceNeedsNoElevatedPrivileges(t *testing.T) {
	verdict, err := queryOnce(context.Background(), startFakeResolver(t))
	if verdict != legReachable {
		t.Fatalf("queryOnce verdict = %s (err %v), want reachable against a real resolver", verdict, err)
	}
}

// The whole point of validating the reply: a captive portal accepts the
// connection on any port. A bare handshake would have called this "reachable"
// and let the client keep retrying Apple for the entire portal period.
func TestQueryOnceRejectsACaptivePortal(t *testing.T) {
	for name, respond := range map[string]func(net.Conn){
		"http redirect":   func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 302 Found\r\nLocation: /login\r\n\r\n")) },
		"accept and hang": func(c net.Conn) { time.Sleep(50 * time.Millisecond) },
		"immediate close": func(c net.Conn) {},
		"echo the query": func(c net.Conn) {
			var prefix [2]byte
			if _, err := readFull(c, prefix[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(prefix[:]))
			if _, err := readFull(c, body); err != nil {
				return
			}
			_, _ = c.Write(append(prefix[:], body...)) // QR still 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				respond(conn)
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			verdict, _ := queryOnce(ctx, listener.Addr().String())
			if verdict != legUnreachable {
				t.Fatalf("verdict = %s, want unreachable — a portal must never read as Internet access", verdict)
			}
		})
	}
}

func TestQueryOnceRejectsAMismatchedTransactionID(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the query and answer it with every check satisfied EXCEPT the
		// id, which is flipped bit-for-bit so it can never match by chance. A
		// reader that has lost the id check accepts this reply as Reachable,
		// which is what makes the test fail without that check — an earlier
		// version retried on a deadline and passed by timing out instead.
		query := make([]byte, 19)
		if _, err := readFull(conn, query); err != nil {
			return
		}
		id := binary.BigEndian.Uint16(query[2:4]) ^ 0xFFFF
		reply := make([]byte, 0, 14)
		reply = binary.BigEndian.AppendUint16(reply, 12)
		reply = binary.BigEndian.AppendUint16(reply, id)
		reply = append(reply, 0x81, 0x80)
		reply = binary.BigEndian.AppendUint16(reply, 1)
		reply = binary.BigEndian.AppendUint16(reply, 0)
		reply = binary.BigEndian.AppendUint16(reply, 0)
		reply = binary.BigEndian.AppendUint16(reply, 0)
		_, _ = conn.Write(reply)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	verdict, err := queryOnce(ctx, listener.Addr().String())
	if verdict != legUnreachable || !errors.Is(err, errHijacked) {
		t.Fatalf("verdict = %s, err = %v; want unreachable with errHijacked — a reply whose id does not match our query must be rejected by the id check itself, not by a timeout", verdict, err)
	}
}

func TestQueryOnceClassifiesAnUnroutableAddressAsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// 203.0.113.0/24 is TEST-NET-3: reserved and never routed, so this can only
	// fail — and it must fail as "unreachable", not as "probe unusable".
	verdict, _ := queryOnce(ctx, "203.0.113.1:53")
	if verdict != legUnreachable {
		t.Fatalf("verdict = %s, want unreachable for an unroutable address (a timeout is a network verdict, not a missing route)", verdict)
	}
}

func TestLocallyBlockedSeparatesHostDenialsFromNetworkVerdicts(t *testing.T) {
	for _, errno := range []syscall.Errno{
		syscall.EACCES, syscall.EPERM, syscall.EMFILE, syscall.ENFILE, syscall.EAFNOSUPPORT,
	} {
		if !locallyBlocked(fmt.Errorf("dial tcp: socket: %w", errno)) {
			t.Errorf("locallyBlocked(%v) = false, want true — a host denial must never read as an outage", errno)
		}
	}
	// These are real routing verdicts and must stay Unreachable; an IPv6-only
	// host is handled by probing IPv6 literals, not by reclassifying these.
	for _, errno := range []syscall.Errno{
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.ECONNREFUSED,
	} {
		if locallyBlocked(fmt.Errorf("dial tcp: %w", errno)) {
			t.Errorf("locallyBlocked(%v) = true, want false — this is a network verdict", errno)
		}
	}
}

func TestQueryProviderReportsBlockedWhenTheCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Bridge shutdown canceling a probe is not an outage verdict.
	outcome, _ := queryProvider(ctx, []string{"203.0.113.1"})
	if outcome != OutcomeBlocked {
		t.Fatalf("outcome = %s, want blocked when the caller canceled", outcome)
	}
}

func TestStabilityRequiresFullContinuousWindow(t *testing.T) {
	const window = 60 * time.Second
	start := time.Unix(1_000, 0)
	var stability Stability

	if stability.Observe(start, true, window) {
		t.Fatal("first successful sample must start, not complete, stabilization")
	}
	if stability.Observe(start.Add(window-time.Second), true, window) {
		t.Fatal("stability completed before the full window")
	}
	if !stability.Observe(start.Add(window), true, window) {
		t.Fatal("stability did not complete at the full window")
	}
}

func TestStabilityFailureResetsWindow(t *testing.T) {
	const window = 60 * time.Second
	start := time.Unix(2_000, 0)
	var stability Stability

	stability.Observe(start, true, window)
	stability.Observe(start.Add(50*time.Second), false, window)
	if stability.Observe(start.Add(70*time.Second), true, window) {
		t.Fatal("first success after failure must begin a new window")
	}
	if stability.Observe(start.Add(129*time.Second), true, window) {
		t.Fatal("reset window completed too early")
	}
	if !stability.Observe(start.Add(130*time.Second), true, window) {
		t.Fatal("reset window did not complete after 60 seconds")
	}
}
