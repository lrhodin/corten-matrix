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

	// And over both methods: TLS on 443 first (the primary proof), DNS on 53
	// as the fallback for hosts whose 443 is blocked or cannot validate.
	legs := probeLegs()
	if len(legs) != 2 || legs[0].name != "tls" || legs[0].port != "443" || legs[1].name != "dns" || legs[1].port != "53" {
		t.Fatalf("probe legs = %+v, want tls:443 then dns:53 (port 80 is deliberately not a leg)", legs)
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
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "internetprobe fixture"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
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

// useLegs points the two legs at fixture addresses (both on 127.0.0.1) and
// installs roots as the TLS trust store; nil roots means the system roots and
// an empty pool means a host with no CA bundle at all.
func useLegs(t *testing.T, tlsAddr, dnsAddr string, roots *x509.CertPool) {
	t.Helper()
	savedTLS, savedDNS, savedRoots := tlsPort, dnsPort, tlsRootCAs
	t.Cleanup(func() { tlsPort, dnsPort, tlsRootCAs = savedTLS, savedDNS, savedRoots })
	_, tlsPort, _ = net.SplitHostPort(tlsAddr)
	_, dnsPort, _ = net.SplitHostPort(dnsAddr)
	tlsRootCAs = roots
}

// The primary proof: only a certificate that chains to a trusted root AND names
// the dialed IP counts. Every other handshake outcome abstains — it must not
// become a verdict, because a MITM portal and a missing CA bundle look alike.
func TestTLSLegAcceptsOnlyACertificateThatValidatesForTheDialedIP(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	server := startFixtureTLS(t, cert)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("verified IP SAN is reachable", func(t *testing.T) {
		useLegs(t, server, server, pool)
		if outcome, err := tlsOnce(ctx, server); outcome != OutcomeReachable {
			t.Fatalf("outcome = %s (err %v), want reachable for a certificate that validates for the dialed IP", outcome, err)
		}
	})
	t.Run("self-signed with the system roots abstains", func(t *testing.T) {
		useLegs(t, server, server, nil)
		outcome, err := tlsOnce(ctx, server)
		if outcome != OutcomeBlocked || !errors.Is(err, errTLSUnverified) {
			t.Fatalf("outcome = %s, err = %v; a portal's self-signed certificate must abstain, never count as reachable", outcome, err)
		}
	})
	t.Run("no CA bundle at all abstains", func(t *testing.T) {
		useLegs(t, server, server, x509.NewCertPool())
		outcome, err := tlsOnce(ctx, server)
		if outcome != OutcomeBlocked || !errors.Is(err, errTLSUnverified) {
			t.Fatalf("outcome = %s, err = %v; an empty trust store must abstain, not vote", outcome, err)
		}
	})
	t.Run("a trusted certificate for a different IP abstains", func(t *testing.T) {
		otherCert, otherPool := fixtureCert(t, net.ParseIP("203.0.113.1"))
		otherServer := startFixtureTLS(t, otherCert)
		useLegs(t, otherServer, otherServer, otherPool)
		outcome, err := tlsOnce(ctx, otherServer)
		if outcome != OutcomeBlocked || !errors.Is(err, errTLSUnverified) {
			t.Fatalf("outcome = %s, err = %v; a certificate that does not name the dialed IP proves nothing", outcome, err)
		}
	})
}

// The provider verdict across both legs, in the cases that matter operationally.
func TestProviderVerdictCombinesTheTLSAndDNSLegs(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	tlsServer := startFixtureTLS(t, cert)
	resolver := startFakeResolver(t)
	refused := closedPort(t)
	hijacker := startHijacker(t)
	host := []string{"127.0.0.1"}

	for _, tc := range []struct {
		name    string
		tlsAddr string
		dnsAddr string
		roots   *x509.CertPool
		want    Outcome
		wantErr error
	}{
		{"verified TLS alone is reachable even with 53 refused", tlsServer, refused, pool, OutcomeReachable, nil},
		{"443 refused but 53 answers is reachable", refused, resolver, nil, OutcomeReachable, nil},
		{"no CA bundle still gets its verdict from 53", tlsServer, resolver, x509.NewCertPool(), OutcomeReachable, nil},
		{"a self-signed portal on 443 with 53 refused is unreachable", tlsServer, refused, nil, OutcomeUnreachable, errTLSUnverified},
		{"no CA bundle with 53 hijacked is unreachable", tlsServer, hijacker, x509.NewCertPool(), OutcomeUnreachable, errHijacked},
		{"both refused is unreachable", refused, refused, nil, OutcomeUnreachable, syscall.ECONNREFUSED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useLegs(t, tc.tlsAddr, tc.dnsAddr, tc.roots)
			outcome, err := queryProvider(context.Background(), host)
			if outcome != tc.want {
				t.Fatalf("outcome = %s (err %v), want %s", outcome, err, tc.want)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want it to carry %v so the log explains the losing leg", err, tc.wantErr)
			}
		})
	}
}

// startHijacker accepts on a port and answers with HTTP, like a captive portal
// intercepting 53.
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

// Only dial-level failures classify, identically for both legs: no route is an
// outage, a socket the host denied is Blocked.
func TestDialLevelErrorsDecideTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		want  Outcome
	}{
		{"ENETUNREACH on every leg is unreachable", syscall.ENETUNREACH, OutcomeUnreachable},
		{"EHOSTUNREACH on every leg is unreachable", syscall.EHOSTUNREACH, OutcomeUnreachable},
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
	t.Run("a leg that reached the network outranks one the host denied", func(t *testing.T) {
		saved := dialContext
		t.Cleanup(func() { dialContext = saved })
		dialContext = func(_ context.Context, _, address string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(address)
			if port == tlsPort {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}
			}
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
		}
		if outcome, _ := queryProvider(context.Background(), cloudflareAddresses); outcome != OutcomeUnreachable {
			t.Fatalf("outcome = %s, want unreachable: the 53 leg tested the network even though 443 was denied", outcome)
		}
	})
}

// A host that silently drops 53 must not make every probe take the whole
// probeTimeout once 443 has already proved the provider.
func TestAWinningLegCancelsTheRestOfTheProviderRace(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	cert, pool := fixtureCert(t, loopback)
	tlsServer := startFixtureTLS(t, cert)
	// A distinct 53 address, so the seam below can blackhole that leg alone.
	useLegs(t, tlsServer, closedPort(t), pool)

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
	outcome, err := queryOnce(context.Background(), startFakeResolver(t))
	if outcome != OutcomeReachable {
		t.Fatalf("queryOnce outcome = %s (err %v), want reachable against a real resolver", outcome, err)
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
			outcome, _ := queryOnce(ctx, listener.Addr().String())
			if outcome != OutcomeUnreachable {
				t.Fatalf("outcome = %s, want unreachable — a portal must never read as Internet access", outcome)
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
	outcome, err := queryOnce(ctx, listener.Addr().String())
	if outcome != OutcomeUnreachable || !errors.Is(err, errHijacked) {
		t.Fatalf("outcome = %s, err = %v; want unreachable with errHijacked — a reply whose id does not match our query must be rejected by the id check itself, not by a timeout", outcome, err)
	}
}

func TestQueryOnceClassifiesAnUnroutableAddressAsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// 203.0.113.0/24 is TEST-NET-3: reserved and never routed, so this can only
	// fail — and it must fail as "unreachable", not as "probe unusable".
	outcome, _ := queryOnce(ctx, "203.0.113.1:53")
	if outcome != OutcomeUnreachable {
		t.Fatalf("outcome = %s, want unreachable for an unroutable address", outcome)
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
