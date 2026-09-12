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
// Classification table. Every (leg kind, failure mode) pair below names the
// ONLY verdict that leg may contribute; queryProvider's fold is the one place
// leg verdicts become an Outcome, and nothing outside this table has standing.
// Two review rounds found bugs that were exactly a leg or state contributing a
// verdict it had no standing to contribute, so the table is the contract.
//
//	Voting TLS leg (443 or 853)
//	  dial refused by this host (EACCES/EPERM/EMFILE/ENFILE/EAFNOSUPPORT) . abstain
//	  dial: no route in this family (ENETUNREACH/EHOSTUNREACH/EADDRNOTAVAIL) no-route
//	  dial: refused, timed out, reset, any other network error ........... unreachable
//	  handshake verified for the dialed IP ............................. reachable
//	  handshake: x509.UnknownAuthorityError ............................ abstain
//	      (a self-signed portal and a missing, stale or malformed local
//	      trust store raise the same error; the pool's contents are no
//	      evidence either way, so this abstains UNCONDITIONALLY)
//	  handshake: x509.CertificateInvalidError with Reason Expired ....... abstain
//	      (clock skew on this host is indistinguishable from an expired
//	      portal certificate)
//	  handshake: chain validates but names another host (HostnameError),
//	      peer does not speak TLS, EOF/reset mid-handshake, any other
//	      protocol or certificate error .................................. unreachable
//	      (positively diagnostic of interception: the providers always
//	      complete a valid handshake on these ports)
//	Diagnostic DNS leg (53)
//	  every outcome ..................................................... abstain
//	      (its result is a note in the joined error, never a verdict)
//
//	Fold, per provider, over all legs of both address families:
//	  any leg reachable ............................................... Reachable
//	  else any leg unreachable ........................................ Unreachable
//	  else every voting leg no-route (no family has a route at all) ... Unreachable
//	  else (only abstentions, or abstentions plus one family's no-route) Blocked
//
// The no-route rule is what keeps a v4-only host honest: its v6 dials fail
// with no route, which says nothing about the Internet, so they cannot outvote
// v4 legs that abstained; they only count once no family reached the network.
// The cross-provider fold in ProbeWith is unchanged: either provider Reachable
// is Reachable, both Blocked is Blocked, anything else is Unreachable.
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

// legVerdict is what one leg may contribute to its provider's verdict. The
// fold in queryProvider is the ONLY place these become an Outcome; the package
// doc's classification table says which failure yields which.
type legVerdict uint8

const (
	// legAbstain: no standing. A diagnostic leg whatever it saw, a socket this
	// host denied, or a handshake failure that may lie with this host.
	// Contributes nothing in either direction.
	legAbstain legVerdict = iota
	// legNoRoute: this address family has no route on this host. Evidence about
	// the host's stacks, not the Internet — unless every voting leg of every
	// family says so.
	legNoRoute
	// legUnreachable: the network was reached and the provider was not there,
	// or the peer is provably not the provider.
	legUnreachable
	// legReachable: an authenticated handshake with the provider.
	legReachable
)

func (v legVerdict) String() string {
	switch v {
	case legReachable:
		return "reachable"
	case legUnreachable:
		return "unreachable"
	case legNoRoute:
		return "no-route"
	default:
		return "abstain"
	}
}

// probeMethod is one leg of a provider's race. Only a voting leg's verdict
// reaches the fold; a diagnostic leg always abstains, and its result survives
// only as a note in the joined error.
type probeMethod struct {
	name   string
	port   string
	voting bool
	run    func(ctx context.Context, address string) (legVerdict, error)
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
var errDiagnosticAnswered = errors.New("answered (diagnostic leg, contributes no verdict)")

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

// queryProvider races every (method, address) leg of a provider and folds
// their verdicts by the package doc's table. The first reachable leg cancels
// the rest, and cancellation reaches an already-connected socket (see
// cancelOnDone), so a won or canceled race returns promptly rather than
// waiting out probeTimeout. Every leg's error is kept (joined, in
// method-then-address order) so a log line shows why each leg lost, including
// a diagnostic leg's note and an abstaining leg's reason.
func queryProvider(ctx context.Context, addresses []string) (Outcome, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	legs := probeLegs()
	type legResult struct {
		index   int
		verdict legVerdict
		err     error
	}
	total := len(legs) * len(addresses)
	results := make(chan legResult, total)
	votingLegs := 0
	for mi, method := range legs {
		if method.voting {
			votingLegs += len(addresses)
		}
		for ai, address := range addresses {
			index := mi*len(addresses) + ai
			go func() {
				verdict, err := method.run(probeCtx, net.JoinHostPort(address, method.port))
				if !method.voting {
					// A diagnostic leg has no standing in either direction.
					if verdict == legReachable {
						err = errDiagnosticAnswered
					}
					verdict = legAbstain
				}
				if err != nil {
					err = fmt.Errorf("%s:%s %s (%s): %w", method.name, method.port, address, verdict, err)
				}
				results <- legResult{index: index, verdict: verdict, err: err}
			}()
		}
	}

	var reachable, unreachable, noRoute int
	errs := make([]error, total)
	for range total {
		leg := <-results
		errs[leg.index] = leg.err
		switch leg.verdict {
		case legReachable:
			reachable++
			// Nothing can strengthen the verdict now; stop waiting on the
			// legs that are still timing out. They are still drained.
			cancel()
		case legUnreachable:
			unreachable++
		case legNoRoute:
			noRoute++
		}
	}
	if reachable > 0 {
		return OutcomeReachable, nil
	}
	// A caller canceling us (bridge shutdown) is not an outage verdict. Our own
	// probeTimeout firing is, and that one leaves ctx.Err() nil.
	if ctx.Err() != nil {
		return OutcomeBlocked, ctx.Err()
	}
	if unreachable > 0 || (noRoute > 0 && noRoute == votingLegs) {
		return OutcomeUnreachable, errors.Join(errs...)
	}
	return OutcomeBlocked, errors.Join(errs...)
}

// classifyDialError is the ONLY place a dial failure becomes a leg verdict, and
// both methods share it so they cannot classify the same errno differently:
// a socket this host refused us abstains, a family with no route is no-route,
// anything else the network did is unreachable.
func classifyDialError(err error) (legVerdict, error) {
	switch {
	case locallyBlocked(err):
		return legAbstain, err
	case noRoute(err):
		return legNoRoute, err
	default:
		return legUnreachable, err
	}
}

// noRoute reports a dial that this host could not even start because the
// address family has no route or no address here — the shape a v6 literal
// takes on a v4-only host (ENETUNREACH on Linux, EHOSTUNREACH on macOS).
func noRoute(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

// cancelOnDone wires ctx cancellation to an already-connected socket: a
// canceled leg would otherwise block in its read until the deadline it set
// from ctx.Deadline(), so a race won by another leg — or a probe canceled by
// bridge shutdown — waited out the whole probeTimeout. The returned stop must
// be deferred.
func cancelOnDone(ctx context.Context, conn net.Conn) (stop func() bool) {
	return context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
}

// errTLSUnverified marks a TLS leg that ABSTAINS: the TCP connection succeeded
// but the handshake failed for a reason that may lie with this host rather
// than the peer — a chain no root here signs (which a self-signed portal and a
// missing, stale or malformed local trust store raise alike), or a certificate
// this host's clock considers expired.
var errTLSUnverified = errors.New("TLS handshake could not be validated on this host (untrusted chain: a portal or this host's CA bundle; or a certificate this host's clock rejects)")

// errTLSRejected marks a TLS leg that reached a peer which is provably not the
// provider: a certificate that validates but names another host (a portal with
// a real certificate for its own domain), a peer that does not speak TLS, a
// connection cut mid-handshake, or any other protocol error.
var errTLSRejected = errors.New("TLS peer is not the provider (captive portal or interception)")

// classifyHandshakeError sorts a failed handshake into abstain or unreachable
// per the package doc's table. It never consults the trust store's contents:
// a non-empty pool can still be stale or malformed, so an untrusted chain is
// ambiguous whatever the pool looks like.
func classifyHandshakeError(err error) (legVerdict, error) {
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownAuthority):
		return legAbstain, fmt.Errorf("%w: %v", errTLSUnverified, err)
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return legAbstain, fmt.Errorf("%w: %v", errTLSUnverified, err)
	default:
		return legUnreachable, fmt.Errorf("%w: %v", errTLSRejected, err)
	}
}

// tlsOnce is a voting leg: a TLS handshake (on 443 or 853) validated against
// the dialed IP literal. With ServerName set to the literal, Go checks the
// certificate's IP SANs, so no name resolution is involved.
func tlsOnce(ctx context.Context, address string) (legVerdict, error) {
	conn, err := dialContext(ctx, "tcp", address)
	if err != nil {
		return classifyDialError(err)
	}
	defer conn.Close()
	defer cancelOnDone(ctx, conn)()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return legAbstain, err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		RootCAs:    tlsRootCAs,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return classifyHandshakeError(err)
	}
	return legReachable, nil
}

// errHijacked marks a connection that completed but did not carry a real DNS
// reply — the captive-portal signature.
var errHijacked = errors.New("connected but no valid DNS reply (captive portal or interception)")

// queryOnce is the diagnostic leg: a DNS-over-TCP query on 53 whose reply must
// carry our transaction id. That check defeats a portal that speaks HTTP on 53
// but not one that echoes our query, which is why queryProvider never lets its
// verdict count in either direction (see probeMethod.voting).
func queryOnce(ctx context.Context, address string) (legVerdict, error) {
	conn, err := dialContext(ctx, "tcp", address)
	if err != nil {
		return classifyDialError(err)
	}
	defer conn.Close()
	defer cancelOnDone(ctx, conn)()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	//nolint:gosec // Not cryptographic: the id only has to be unguessable enough
	// that a stale or echoed reply on this one connection does not match.
	id := uint16(rand.Intn(1 << 16))
	if _, err := conn.Write(dnsRootNSQuery(id)); err != nil {
		return legUnreachable, err
	}
	if err := readDNSReply(conn, id); err != nil {
		return legUnreachable, err
	}
	return legReachable, nil
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
