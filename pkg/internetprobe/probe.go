// Package internetprobe provides the bridge's Apple-independent Internet
// reachability check and recovery-stability tracker.
//
// The probe contacts two public providers (Cloudflare and Google) at literal
// addresses and demands proof that it reached the real host. The selection
// criterion for a leg that may vote Reachable is AUTHENTICATION, not port
// availability: a verdict of "up" authorizes contacting Apple, so every leg
// that can produce it must be something an intermediary cannot forge. Two such
// legs are raced per provider and per address family, both a TLS handshake
// validated against the dialed IP literal:
//
//   - TLS on 443 — the primary check. A completed handshake whose certificate
//     chains to a trusted root and carries the dialed IP in its SANs
//     (cloudflare-dns.com lists 1.1.1.1/1.0.0.1 and their IPv6 twins;
//     dns.google lists 8.8.8.8/8.8.4.4 and theirs) is cryptographic proof that
//     we spoke to the provider. 443 is essentially never blocked outbound.
//   - TLS on 853 (the DNS-over-TLS port) — the fallback for a network that
//     does block or intercept 443. Both providers present the same validated
//     certificates there, so it is exactly as unforgeable.
//
// Plaintext DNS on 53 is NOT a voting leg. Its reply validation (transaction
// id, QR bit, QDCOUNT) checks only values an interceptor reads off our own
// query and echoes back, and transparent DNS interception is ordinary on ISP,
// hotel and corporate networks — so it could authorize an Apple reconnect
// behind a portal, the exact hole this package exists to close. It still runs,
// as a DIAGNOSTIC leg: it can never make a provider Reachable, and its result
// appears in the joined error so an operator can tell "no network" (53 fails
// too) from "443/853 filtered" (53 answers). Port 80 was considered and
// rejected for the same reason: a captive portal answers 80 with a redirect,
// and so does 1.1.1.1 itself, so there is nothing to authenticate. Do not add
// either as a voting leg.
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
// How the legs combine is what keeps the verdict honest. Dial-level failures
// classify as they always have: a refused or unroutable connection is
// Unreachable, and a socket the host would not give us is Blocked (see
// locallyBlocked). A handshake that reaches a peer which is provably NOT the
// provider — a certificate that validates but names another host, a peer that
// does not speak TLS at all, a connection reset mid-handshake — is Unreachable.
// Only two handshake failures ABSTAIN (Blocked, "no evidence") instead of
// voting: a certificate this host cannot chain to any root when the host has NO
// trust store at all (a minimal container without ca-certificates — with a
// trust store present, an untrusted chain is a portal and votes Unreachable),
// and a certificate the host considers invalid on time grounds (clock skew on
// the host is indistinguishable from an expired portal cert). A host that
// cannot validate anything therefore reads as Blocked — the probe cannot run
// here — and the recovery loop's unusable-probe hatch, not a forged verdict,
// decides what happens next, with the cert failure named in the log.
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

// Ports of the probe legs. Vars only so a test can point a leg at a fixture
// listener; never reassigned in production. See the package doc for why 53 is
// diagnostic-only and 80 is not, and must not become, a leg.
var (
	tlsPort = "443"
	dotPort = "853"
	dnsPort = "53"
)

// probeMethod is one leg of a provider's race. Only a voting leg can make the
// provider Reachable; a diagnostic leg's success is recorded in the joined
// error and contributes nothing to the verdict, while its dial-level and
// hijack failures still count (they can only keep the verdict down).
type probeMethod struct {
	name   string
	port   string
	voting bool
	run    func(ctx context.Context, address string) (Outcome, error)
}

// probeLegs lists the legs raced against every address of a provider, the
// primary first. Built per call so the port seams are read at probe time.
func probeLegs() []probeMethod {
	return []probeMethod{
		{name: "tls", port: tlsPort, voting: true, run: tlsOnce},
		{name: "dot", port: dotPort, voting: true, run: tlsOnce},
		{name: "dns", port: dnsPort, voting: false, run: queryOnce},
	}
}

// errDiagnosticAnswered notes, in a provider's joined error, that the
// diagnostic 53 leg got a valid reply even though the voting legs did not
// prove the host: the signature of a network that filters 443/853 or of a
// DNS interceptor, either way not a reason to contact Apple.
var errDiagnosticAnswered = errors.New("answered (diagnostic leg, cannot vote Reachable)")

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
				if !method.voting && outcome == OutcomeReachable {
					// A diagnostic leg's success is a note, never a vote.
					outcome, err = OutcomeBlocked, errDiagnosticAnswered
				}
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

// errTLSUnverified marks a TLS leg that ABSTAINS: the TCP connection succeeded
// but this host could not validate the certificate for a reason that may lie
// with the host rather than the peer (no trust store at all, or a certificate
// it considers invalid on time grounds). OutcomeBlocked, "no evidence".
var errTLSUnverified = errors.New("TLS handshake could not be validated on this host (no usable CA bundle, or a certificate this host considers invalid)")

// errTLSRejected marks a TLS leg that reached a peer which is provably not the
// provider: a certificate that validates but names another host (a portal with
// a real certificate for its own domain), a chain no root in a present trust
// store signs (a self-signed portal), a peer that does not speak TLS, or a
// connection cut mid-handshake. OutcomeUnreachable.
var errTLSRejected = errors.New("TLS peer is not the provider (captive portal or interception)")

// hasTrustStore reports whether this host has any root certificates at all
// under the pool the probe validates against. With none, an untrusted chain
// says nothing about the peer; with some, it says the peer is not who it
// claims. Computed once for the system roots.
var (
	systemTrustStoreOnce  sync.Once
	systemTrustStoreKnown bool
)

func hasTrustStore(pool *x509.CertPool) bool {
	if pool != nil {
		return !pool.Equal(x509.NewCertPool())
	}
	systemTrustStoreOnce.Do(func() {
		system, err := x509.SystemCertPool()
		systemTrustStoreKnown = err == nil && system != nil && !system.Equal(x509.NewCertPool())
	})
	return systemTrustStoreKnown
}

// classifyHandshakeError sorts a failed handshake into abstain (Blocked) or a
// verdict (Unreachable) per the package doc.
func classifyHandshakeError(err error, roots *x509.CertPool) (Outcome, error) {
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownAuthority) && !hasTrustStore(roots):
		return OutcomeBlocked, fmt.Errorf("%w: %v", errTLSUnverified, err)
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return OutcomeBlocked, fmt.Errorf("%w: %v", errTLSUnverified, err)
	default:
		return OutcomeUnreachable, fmt.Errorf("%w: %v", errTLSRejected, err)
	}
}

// tlsOnce is a voting leg: a TLS handshake (on 443 or 853) validated against
// the dialed IP literal. With ServerName set to the literal, Go checks the
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
	roots := tlsRootCAs
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return classifyHandshakeError(err, roots)
	}
	return OutcomeReachable, nil
}

// errHijacked marks a connection that completed but did not carry a real DNS
// reply — the captive-portal signature.
var errHijacked = errors.New("connected but no valid DNS reply (captive portal or interception)")

// queryOnce is the diagnostic leg: a DNS-over-TCP query on 53 whose reply must
// carry our transaction id. That check defeats a portal that speaks HTTP on 53
// but not one that echoes our query, which is why queryProvider never lets its
// success vote (see probeMethod.voting).
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
