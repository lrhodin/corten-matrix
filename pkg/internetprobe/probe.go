// Package internetprobe provides the bridge's Apple-independent Internet
// reachability check and recovery-stability tracker.
//
// The probe contacts two public providers (Cloudflare and Google) at literal
// addresses and demands proof that it reached the real host. Two proofs are
// accepted, raced per provider and per address family:
//
//   - TLS on 443 with certificate validation — the primary check. A completed
//     handshake whose certificate chains to a trusted root and carries the
//     dialed IP in its SANs (cloudflare-dns.com lists 1.1.1.1/1.0.0.1 and their
//     IPv6 twins; dns.google lists 8.8.8.8/8.8.4.4 and theirs) is cryptographic
//     proof that we spoke to the provider. A captive portal or a transparently
//     proxying ISP can accept the connection and can even echo a DNS
//     transaction id, but it cannot forge a certificate for 1.1.1.1. And 443
//     is essentially never blocked outbound.
//   - DNS-over-TCP on 53 with reply validation — the fallback. VPS providers
//     commonly block outbound 53 to prevent DNS-amplification abuse, so it is
//     not the primary check any more, but it is what still works on a host
//     whose 443 is blocked or whose TLS stack cannot validate anything. A
//     matching transaction id with QR set proves a real resolver answered.
//
// Port 80 was considered and rejected: a captive portal answers 80 with a
// redirect, and so does 1.1.1.1 itself, so there is nothing to validate and it
// would reopen the exact hole reply validation exists to close. Do not add it.
//
// Three properties are load-bearing, and each replaced something that was
// quietly wrong:
//
//   - It is the same code path on macOS and Linux, the two platforms this bridge
//     runs on, with no per-platform binary to locate, no argument spelling, and
//     no exit-code table to interpret (BSD ping reports "no reply" as exit 2
//     while Linux iputils uses exit 2 for "something else went wrong").
//   - It needs no elevated privileges. ICMP needs a raw socket or the
//     unprivileged-ICMP path, which on Linux depends on CAP_NET_RAW or on
//     net.ipv4.ping_group_range covering the service account's GID. Under an
//     unprivileged container that fails outright, and a probe that can never
//     succeed is the worst possible input to a policy whose "offline" branch
//     tears the client down.
//   - It VALIDATES what comes back. A bare TCP handshake proves only that
//     something accepted a SYN, and a captive portal — the most common "link
//     up, Internet down" case there is — accepts every port. That would have
//     reported Reachable and let the client keep retrying Apple for the whole
//     portal period, which is the exact hazard this package exists to prevent.
//
// How the legs combine is what keeps the verdict honest. Only DIAL-level
// failures classify: a refused or unroutable connection is Unreachable, and a
// socket the host would not give us is Blocked (see locallyBlocked). A TLS
// handshake that completes the TCP connection but fails to validate is
// ambiguous — a MITM portal and a minimal container with no CA bundle produce
// the same x509 error — so that leg ABSTAINS rather than voting, and the 53 leg
// or a dial-level errno decides. A host with no CA bundle therefore still gets
// its verdict from 53. The residual case is a host with no CA bundle whose
// 53 is also blocked: it reads as Unreachable, the recovery loop's ceilings
// bound the cost, and the joined leg errors in the log name the cert failure.
//
// Each provider is tried over IPv4 and IPv6 concurrently. That is not garnish:
// on an IPv6-only or NAT64/DNS64 host there is no IPv4 route at all, so dialing
// only IPv4 literals fails permanently with ENETUNREACH and a healthy bridge
// would read as a permanent outage.
//
// Addresses are literals, and the TLS ServerName is the dialed literal, so the
// probe never depends on the host's own DNS.
package internetprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"syscall"
	"time"
)

const (
	CloudflareTarget = "1.1.1.1"
	GoogleTarget     = "8.8.8.8"

	cloudflareTarget6 = "2606:4700:4700::1111"
	googleTarget6     = "2001:4860:4860::8888"

	// One provider's whole race — every address family and both methods — gets
	// this long. A verified handshake measures ~40ms, so there is ample
	// headroom; the budget is for the leg that has to time out.
	probeTimeout = 3 * time.Second
)

// Ports of the two probe methods. Vars only so a test can point a leg at a
// fixture listener; never reassigned in production. See the package doc for
// why 80 is not, and must not become, a third.
var (
	tlsPort = "443"
	dnsPort = "53"
)

// probeMethod is one way of proving a provider host is real.
type probeMethod struct {
	name string
	port string
	run  func(ctx context.Context, address string) (Outcome, error)
}

// probeLegs lists the methods raced against every address of a provider, the
// primary first. Built per call so the port seams are read at probe time.
func probeLegs() []probeMethod {
	return []probeMethod{
		{name: "tls", port: tlsPort, run: tlsOnce},
		{name: "dns", port: dnsPort, run: queryOnce},
	}
}

// Seams for tests. dialContext lets a test inject dial-level errnos
// (ENETUNREACH, EACCES, EMFILE) deterministically and observe which legs are
// dialed; tlsRootCAs (nil means the system roots) lets a test stand in a
// fixture CA, or an empty pool for a host with no CA bundle. Never reassigned
// in production.
var (
	dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
	tlsRootCAs *x509.CertPool
)

// Outcome classifies one provider's probe. The three cases are not
// interchangeable: Unreachable is evidence of an outage, Blocked is the absence
// of evidence, and a recovery policy that treats Blocked as Unreachable will
// tear the client down on a host where the probe simply cannot run.
//
// The zero value is Blocked so an unpopulated Result never reads as an outage.
type Outcome uint8

const (
	// OutcomeBlocked means the probe never got to test the network: the host
	// denied the socket or had no descriptors left. Says nothing about the
	// Internet.
	OutcomeBlocked Outcome = iota
	// OutcomeUnreachable means the probe ran and no leg could prove it reached
	// the real provider — no route, refused, no reply, or a hijacked
	// connection.
	OutcomeUnreachable
	// OutcomeReachable means a provider host proved itself: a certificate that
	// validates for its IP, or a DNS reply matching our query.
	OutcomeReachable
)

func (o Outcome) String() string {
	switch o {
	case OutcomeReachable:
		return "reachable"
	case OutcomeUnreachable:
		return "unreachable"
	default:
		return "blocked"
	}
}

// Runner probes one provider's addresses. Injectable for deterministic tests.
type Runner func(ctx context.Context, addresses []string) (Outcome, error)

// Result records the outcome of both Apple-independent public probes.
type Result struct {
	Cloudflare    Outcome
	Google        Outcome
	CloudflareErr error
	GoogleErr     error
}

// Reachable reports whether either provider proved reachable.
func (r Result) Reachable() bool {
	return r.Cloudflare == OutcomeReachable || r.Google == OutcomeReachable
}

// Blocked reports that this host would not let the probe run at all, so the
// result carries no information about Internet connectivity. Callers must treat
// it as "unknown" and must never treat it as "Internet down".
func (r Result) Blocked() bool {
	return r.Cloudflare == OutcomeBlocked && r.Google == OutcomeBlocked
}

var (
	cloudflareAddresses = []string{CloudflareTarget, cloudflareTarget6}
	googleAddresses     = []string{GoogleTarget, googleTarget6}
)

// Probe contacts both public providers and reports each result.
func Probe(ctx context.Context) Result {
	return ProbeWith(ctx, queryProvider)
}

// ProbeWith is Probe with an injectable runner for deterministic tests.
// Both providers are always probed, even if one returns successfully first.
func ProbeWith(ctx context.Context, runner Runner) Result {
	type providerResult struct {
		name    string
		outcome Outcome
		err     error
	}
	providers := []struct {
		name      string
		addresses []string
	}{
		{"cloudflare", cloudflareAddresses},
		{"google", googleAddresses},
	}
	results := make(chan providerResult, len(providers))
	var wg sync.WaitGroup
	wg.Add(len(providers))
	for _, provider := range providers {
		go func() {
			defer wg.Done()
			outcome, err := runner(ctx, provider.addresses)
			results <- providerResult{name: provider.name, outcome: outcome, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var result Result
	for probe := range results {
		switch probe.name {
		case "cloudflare":
			result.Cloudflare, result.CloudflareErr = probe.outcome, probe.err
		case "google":
			result.Google, result.GoogleErr = probe.outcome, probe.err
		}
	}
	return result
}

// queryProvider races every (method, address) leg of a provider and keeps the
// strongest verdict. Outcome's ordering makes that a max: any leg that proved
// the host outranks everything, and failing that, any leg that actually reached
// the network outranks one we were never allowed to try — which is also what
// lets an abstaining TLS leg (OutcomeBlocked) defer to the 53 leg. The first
// winning leg cancels the rest: on a host that silently drops 53 the DNS leg
// would otherwise hold a verdict the 443 leg settled in 40ms for the whole
// probeTimeout. Every leg's error is kept (joined, in method-then-address
// order) so a log line shows why each leg lost, including a cert failure that
// did not itself decide anything.
func queryProvider(ctx context.Context, addresses []string) (Outcome, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	legs := probeLegs()
	type legResult struct {
		index   int
		outcome Outcome
		err     error
	}
	total := len(legs) * len(addresses)
	results := make(chan legResult, total)
	for mi, method := range legs {
		for ai, address := range addresses {
			index := mi*len(addresses) + ai
			go func() {
				outcome, err := method.run(probeCtx, net.JoinHostPort(address, method.port))
				if err != nil {
					err = fmt.Errorf("%s:%s %s: %w", method.name, method.port, address, err)
				}
				results <- legResult{index: index, outcome: outcome, err: err}
			}()
		}
	}

	best := OutcomeBlocked
	errs := make([]error, total)
	for range total {
		leg := <-results
		errs[leg.index] = leg.err
		if leg.outcome > best {
			best = leg.outcome
		}
		if best == OutcomeReachable {
			// Nothing can strengthen the verdict now; stop waiting on the
			// legs that are still timing out. They are still drained below.
			cancel()
		}
	}
	if best == OutcomeReachable {
		return OutcomeReachable, nil
	}
	// A caller canceling us (bridge shutdown) is not an outage verdict. Our own
	// probeTimeout firing is, and that one leaves ctx.Err() nil.
	if ctx.Err() != nil {
		return OutcomeBlocked, ctx.Err()
	}
	return best, errors.Join(errs...)
}

// classifyDialError is the ONLY place a probe failure becomes a verdict: a
// socket the host refused us is Blocked (no evidence), anything the network
// did is Unreachable. Both methods share it so they cannot classify the same
// errno differently.
func classifyDialError(err error) (Outcome, error) {
	if locallyBlocked(err) {
		return OutcomeBlocked, err
	}
	return OutcomeUnreachable, err
}

// errTLSUnverified marks a 443 leg whose TCP connection succeeded but whose
// handshake did not produce a certificate that validates for the dialed IP.
// The leg abstains — OutcomeBlocked, "no evidence" — rather than voting
// Unreachable, because a captive portal terminating TLS and a host with no
// usable CA bundle raise the same error and only the 53 leg or a dial-level
// errno can tell them apart.
var errTLSUnverified = errors.New("TLS handshake did not verify the host's certificate for the dialed IP (captive portal, interception, or no usable CA bundle)")

// tlsOnce is the primary leg: a TLS handshake on 443 validated against the
// dialed IP literal. With ServerName set to the literal, Go checks the
// certificate's IP SANs, so no name resolution is involved.
func tlsOnce(ctx context.Context, address string) (Outcome, error) {
	conn, err := dialContext(ctx, "tcp", address)
	if err != nil {
		return classifyDialError(err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return OutcomeBlocked, err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		RootCAs:    tlsRootCAs,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return OutcomeBlocked, fmt.Errorf("%w: %v", errTLSUnverified, err)
	}
	return OutcomeReachable, nil
}

// errHijacked marks a connection that completed but did not carry a real DNS
// reply — the captive-portal signature.
var errHijacked = errors.New("connected but no valid DNS reply (captive portal or interception)")

// queryOnce is the fallback leg: a DNS-over-TCP query on 53 whose reply must
// carry our transaction id.
func queryOnce(ctx context.Context, address string) (Outcome, error) {
	conn, err := dialContext(ctx, "tcp", address)
	if err != nil {
		return classifyDialError(err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	//nolint:gosec // Not cryptographic: the id only has to be unguessable enough
	// that a stale or echoed reply on this one connection does not match.
	id := uint16(rand.Intn(1 << 16))
	if _, err := conn.Write(dnsRootNSQuery(id)); err != nil {
		return OutcomeUnreachable, err
	}
	if err := readDNSReply(conn, id); err != nil {
		return OutcomeUnreachable, err
	}
	return OutcomeReachable, nil
}

// dnsRootNSQuery builds a DNS-over-TCP query for the root NS set: a 2-byte
// length prefix, a 12-byte header with RD set, then the root QNAME and
// QTYPE=NS/QCLASS=IN. Every resolver answers it, and it is 19 bytes on the wire.
func dnsRootNSQuery(id uint16) []byte {
	message := make([]byte, 0, 19)
	message = binary.BigEndian.AppendUint16(message, 17) // TCP length prefix
	message = binary.BigEndian.AppendUint16(message, id)
	message = binary.BigEndian.AppendUint16(message, 0x0100) // RD
	message = binary.BigEndian.AppendUint16(message, 1)      // QDCOUNT
	message = binary.BigEndian.AppendUint16(message, 0)      // ANCOUNT
	message = binary.BigEndian.AppendUint16(message, 0)      // NSCOUNT
	message = binary.BigEndian.AppendUint16(message, 0)      // ARCOUNT
	message = append(message, 0x00)                          // QNAME: root
	message = binary.BigEndian.AppendUint16(message, 2)      // QTYPE: NS
	message = binary.BigEndian.AppendUint16(message, 1)      // QCLASS: IN
	return message
}

// readDNSReply accepts only a reply that is actually a DNS response to the query
// we just sent. A captive portal that accepts the connection and speaks HTTP, or
// echoes bytes back, fails all three checks.
func readDNSReply(conn net.Conn, id uint16) error {
	var lengthPrefix [2]byte
	if _, err := readFull(conn, lengthPrefix[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthPrefix[:])
	if length < 12 || length > 4096 {
		return errHijacked
	}
	header := make([]byte, 12)
	if _, err := readFull(conn, header); err != nil {
		return err
	}
	if binary.BigEndian.Uint16(header[0:2]) != id {
		return errHijacked
	}
	if header[2]&0x80 == 0 { // QR: must be a response
		return errHijacked
	}
	if binary.BigEndian.Uint16(header[4:6]) != 1 { // QDCOUNT: echoes our question
		return errHijacked
	}
	return nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := conn.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// locallyBlocked reports errors that mean this host would not let us open the
// socket, as opposed to the network failing to carry it. These must never be
// reported as an outage: treating them as "Internet down" parks the bridge in
// Apple-free recovery on no evidence.
//
// Note what is deliberately NOT here: ENETUNREACH, EHOSTUNREACH and friends are
// genuine network verdicts and belong in Unreachable. The IPv6-only host that
// would otherwise make ENETUNREACH permanent is handled by probing an IPv6
// literal per provider, not by reclassifying a real routing failure.
// EMFILE/ENFILE are transient rather than permanent, but they are still "we
// could not ask", which is the distinction that matters here.
func locallyBlocked(err error) bool {
	return errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.EAFNOSUPPORT)
}

// Stability tracks successful reachability samples using caller-provided
// monotonic time values. A failed sample always starts the window over.
type Stability struct {
	since time.Time
}

// Observe records one sample and reports whether successful samples span the
// requested stability window.
func (s *Stability) Observe(now time.Time, reachable bool, window time.Duration) bool {
	if !reachable {
		s.since = time.Time{}
		return false
	}
	if s.since.IsZero() {
		s.since = now
		return window <= 0
	}
	return now.Sub(s.since) >= window
}

// Reset discards any partial stability window.
func (s *Stability) Reset() {
	s.since = time.Time{}
}
