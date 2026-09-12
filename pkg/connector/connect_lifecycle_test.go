package connector

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// newLifecycleTestClient builds an IMClient that Connect can be driven through
// up to NewClient without a real rustpush session: no users (so the keystore
// check is skipped), no persisted account (so the token-provider restore is
// skipped), a dummy connection pointer that the closeAPSConnection seam
// intercepts before any FFI call, and a nil bridge-state queue (Send on a nil
// queue is a no-op; the states we care about go through the sendRecoveryState
// seam).
func newLifecycleTestClient() *IMClient {
	return &IMClient{
		Main: &IMConnector{Bridge: &bridgev2.Bridge{
			Config: &bridgeconfig.BridgeConfig{UnknownErrorAutoReconnect: 5 * time.Minute},
		}},
		UserLogin: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "lifecycle-test", Metadata: &UserLoginMetadata{}},
			Log:       zerolog.Nop(),
		},
		connection: &rustpushgo.WrappedApsConnection{},
	}
}

// installLifecycleSeams swaps the FFI-touching seams for recorders and restores
// them afterward. Returns the recorded closes and sent states, both guarded.
func installLifecycleSeams(t *testing.T) (closed *int32, states *[]status.BridgeState, mu *sync.Mutex) {
	t.Helper()
	newClient, closeConn, send, keystore := newRustClient, closeAPSConnection, sendRecoveryState, validateKeystore
	t.Cleanup(func() {
		newRustClient, closeAPSConnection, sendRecoveryState, validateKeystore = newClient, closeConn, send, keystore
	})
	closed = new(int32)
	states = new([]status.BridgeState)
	mu = new(sync.Mutex)
	closeAPSConnection = func(conn *rustpushgo.WrappedApsConnection) {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil {
			*closed++
		}
	}
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		mu.Lock()
		defer mu.Unlock()
		*states = append(*states, st)
		return true
	}
	return closed, states, mu
}

// Mutations 4, 5, 6 and 16: the NewClient-failure return must close the APS
// connection LoadUserLogin built, close the epoch so the lifecycle predicates
// are honest, and ask bridgev2 to retry with StateUnknownError rather than
// terminate the login with StateBadCredentials.
func TestConnectAbandonsTheEpochWhenNewClientFails(t *testing.T) {
	closed, states, mu := installLifecycleSeams(t)
	newRustClient = func(*rustpushgo.WrappedApsConnection, *rustpushgo.WrappedIdsUsers, *rustpushgo.WrappedIdsngmIdentity, *rustpushgo.WrappedOsConfig, **rustpushgo.WrappedTokenProvider, rustpushgo.MessageCallback, rustpushgo.UpdateUsersCallback) (*rustpushgo.Client, error) {
		return nil, errors.New("synthetic client failure")
	}

	client := newLifecycleTestClient()
	client.Connect(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if *closed != 1 {
		t.Fatalf("APS connection closed %d times, want 1", *closed)
	}
	if len(*states) != 1 {
		t.Fatalf("states = %v, want exactly one", *states)
	}
	if st := (*states)[0]; st.StateEvent != status.StateUnknownError || st.Error != "im-client-create-failed" {
		t.Fatalf("state = %s/%s, want StateUnknownError/im-client-create-failed so bridgev2 retries", st.StateEvent, st.Error)
	}
	if client.connectEpochActive() {
		t.Fatal("connectEpochActive reports a live epoch for a client with no Rust client")
	}
	if client.hasOrphanedRecovery() {
		t.Fatal("hasOrphanedRecovery claims a recovery loop that was never started")
	}
	if client.client != nil {
		t.Fatal("a failed NewClient must not install a client")
	}
}

// Mutations 15 and 16: the two lifecycle predicates must agree with what the
// epoch actually is at every stage of its life.
func TestConnectEpochPredicatesAreHonest(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)

	fresh := func() *IMClient {
		c := newLifecycleTestClient()
		c.stopChan = make(chan struct{})
		c.recoveryDone = make(chan struct{})
		c.recoveryBridgeState = &bridgev2.BridgeStateQueue{}
		return c
	}
	check := func(t *testing.T, c *IMClient, stage string, active, orphaned bool) {
		t.Helper()
		if got := c.connectEpochActive(); got != active {
			t.Errorf("%s: connectEpochActive = %v, want %v", stage, got, active)
		}
		if got := c.hasOrphanedRecovery(); got != orphaned {
			t.Errorf("%s: hasOrphanedRecovery = %v, want %v", stage, got, orphaned)
		}
	}

	check(t, &IMClient{}, "never connected", false, false)

	c := fresh()
	check(t, c, "live epoch", true, false)

	c.disconnectForInternetRecovery()
	check(t, c, "torn down by recovery with the loop armed", false, true)
	c.Disconnect()
	check(t, c, "recovery retired", false, false)

	c = fresh()
	c.abandonConnectEpoch(status.BridgeState{StateEvent: status.StateUnknownError})
	check(t, c, "abandoned before any worker started", false, false)

	c = fresh()
	c.Disconnect()
	check(t, c, "normal lifecycle teardown", false, false)
}

// Mutations 3 and 10, revised in round 8: evidence of a working receive path
// (the wedge watchdog seeing an inbound frame) or a logout are the only things
// that clear the hand-back run. markConnected must NOT: every rebuild reaches
// it whether or not APS came up, so clearing there meant the backoff never
// widened for any rebuild source.
func TestHandBackRunIsClearedOnlyByAReceivedFrameOrLogout(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	now := time.Unix(90_000, 0)

	client := newLifecycleTestClient()
	client.Main.noteHandBack(client.UserLogin.ID, now)
	if _, ok := client.Main.handBackDue(client.UserLogin.ID, now.Add(time.Minute)); ok {
		t.Fatal("precondition: the run should be holding")
	}
	if !client.markConnected() {
		t.Fatal("markConnected refused with no teardown pending")
	}
	if _, ok := client.Main.handBackDue(client.UserLogin.ID, now.Add(time.Minute)); ok {
		t.Fatal("markConnected cleared the hand-back run, but reaching it proves nothing about the courier connection")
	}

	client.LogoutRemote(context.Background())
	if _, ok := client.Main.handBackDue(client.UserLogin.ID, now.Add(time.Minute)); !ok {
		t.Fatal("LogoutRemote must clear the hand-back run")
	}
}

// Round-8 finding 1, the commit-point half: Disconnect flags the client BEFORE
// it waits on lifecycleMu, so a Connect that is still running can see it has
// been retired. Tested with the lock held, exactly as a running Connect holds
// it: the flag must appear while Disconnect is still blocked.
func TestDisconnectFlagsTerminationBeforeWaitingForConnect(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	client := newLifecycleTestClient()
	client.stopChan = make(chan struct{})
	client.recoveryDone = make(chan struct{})
	release := holdLifecycleLock(client)

	done := make(chan struct{})
	go func() {
		client.Disconnect()
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !client.terminationRequested() {
		if time.Now().After(deadline) {
			release()
			t.Fatal("Disconnect did not flag termination while blocked behind the Connect lifecycle lock")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		release()
		t.Fatal("Disconnect finished while the Connect lifecycle lock was held")
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not finish once the lock was released")
	}
}

// Round-8 finding 1, the Connect half: a Connect whose client was retired while
// NewClient was running must stop at the commit point right after it — before
// the IDS sweep, StatusKit init, StateConnected and every epoch worker — and
// leave the client for the pending Disconnect to stop and destroy. Driven
// through the real Connect: the NewClient seam simulates a Disconnect landing
// mid-build by flagging the client before it returns.
func TestConnectAbandonsItselfWhenRetiredDuringNewClient(t *testing.T) {
	closed, states, mu := installLifecycleSeams(t)
	client := newLifecycleTestClient()
	newRustClient = func(*rustpushgo.WrappedApsConnection, *rustpushgo.WrappedIdsUsers, *rustpushgo.WrappedIdsngmIdentity, *rustpushgo.WrappedOsConfig, **rustpushgo.WrappedTokenProvider, rustpushgo.MessageCallback, rustpushgo.UpdateUsersCallback) (*rustpushgo.Client, error) {
		client.lifecycleTerminated.Store(true)
		// Never touched: Connect must return before any call on it, and the
		// test detaches it below so no teardown reaches FFI.
		return &rustpushgo.Client{}, nil
	}
	// Anything past the commit point clears this run (via the watchdog) or
	// sends StateConnected; neither may happen.
	client.Main.noteHandBack(client.UserLogin.ID, time.Now())

	finished := make(chan struct{})
	go func() {
		client.Connect(context.Background())
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return; it is running the post-NewClient setup against a dummy client")
	}
	if client.client == nil {
		t.Fatal("the built client must be left installed for the pending Disconnect to stop and destroy")
	}
	client.client = nil // detach the dummy before any teardown can reach FFI
	if !client.connectEpochActive() {
		t.Fatal("an abandoned Connect must leave the epoch to the pending Disconnect, not close it itself")
	}
	if _, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now()); ok {
		t.Fatal("the hand-back run was cleared: Connect ran past its commit point")
	}
	mu.Lock()
	sent, closes := len(*states), *closed
	mu.Unlock()
	if sent != 0 || closes != 0 {
		t.Fatalf("an abandoned Connect sent %d states and closed %d connections; the pending Disconnect owns both", sent, closes)
	}
	client.Disconnect() // the teardown the flag stood in for; must complete cleanly
	if client.connectEpochActive() {
		t.Fatal("Disconnect did not close the epoch")
	}
}

// The last commit point: a teardown requested during the setup tail is caught
// at markConnected, which then sends nothing. The send is observed through the
// seam, so reordering it ahead of the check is caught (round-9 survivor).
func TestMarkConnectedRefusesOnceTerminationIsRequested(t *testing.T) {
	_, states, mu := installLifecycleSeams(t)
	client := newLifecycleTestClient()
	client.recoveryBridgeState = &bridgev2.BridgeStateQueue{}
	client.UserLogin.BridgeState = client.recoveryBridgeState

	if !client.markConnected() {
		t.Fatal("markConnected refused with no teardown pending")
	}
	mu.Lock()
	sent := len(*states)
	last := status.BridgeState{}
	if sent > 0 {
		last = (*states)[sent-1]
	}
	mu.Unlock()
	if sent != 1 || last.StateEvent != status.StateConnected {
		t.Fatalf("a live client must send exactly one StateConnected, got %d states (last %+v)", sent, last)
	}

	client.lifecycleTerminated.Store(true)
	if client.markConnected() {
		t.Fatal("markConnected reported a connection for a client whose teardown is pending")
	}
	mu.Lock()
	sent = len(*states)
	mu.Unlock()
	if sent != 1 {
		t.Fatalf("a retired client sent StateConnected anyway (%d states): the send must come after the check", sent)
	}
}

// newTestBridgeDB builds an in-memory-equivalent bridgev2 database with the
// bridge's real schema and metadata types, for the Connect stages that read or
// write the KV store and the user_login row.
func newTestBridgeDB(t *testing.T) *database.Database {
	t.Helper()
	db := database.New("imessage-test", (&IMConnector{}).GetDBMetaTypes(), newTestSQLiteDB(t))
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade bridge schema: %v", err)
	}
	return db
}

// Round-9 blocker: a retired Connect must never stamp im.last_full_connect.
// The replacement's Connect starts immediately and reads that key, so a stamp
// from the dying client makes the SURVIVING client skip its StatusKit invite
// sweep for the whole cooldown. The gate is one function so the termination
// check and the stamp cannot be separated.
func TestFullConnectSweepGateStampsOnlyForALiveClient(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	db := newTestBridgeDB(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	stamp := func() string { return db.KV.Get(ctx, lastFullConnectKVKey) }

	client := newLifecycleTestClient()
	client.Main.Bridge.DB = db
	if skip, retired := client.gateFullConnectIDSSweep(zerolog.Nop(), now); skip || retired {
		t.Fatalf("first connect: skip=%v retired=%v, want the sweep to run", skip, retired)
	}
	if stamp() != now.Format(time.RFC3339) {
		t.Fatalf("first connect did not stamp the key: %q", stamp())
	}
	inside := now.Add(fullConnectIDSCooldown - time.Minute)
	if skip, retired := client.gateFullConnectIDSSweep(zerolog.Nop(), inside); !skip || retired {
		t.Fatalf("inside the cooldown: skip=%v retired=%v, want skip", skip, retired)
	}
	if stamp() != now.Format(time.RFC3339) {
		t.Fatalf("a skipped sweep re-stamped the key: %q", stamp())
	}
	past := now.Add(fullConnectIDSCooldown + time.Minute)
	if skip, retired := client.gateFullConnectIDSSweep(zerolog.Nop(), past); skip || retired {
		t.Fatalf("past the cooldown: skip=%v retired=%v, want the sweep to run", skip, retired)
	}
	if stamp() != past.Format(time.RFC3339) {
		t.Fatalf("a sweep past the cooldown did not stamp the key: %q", stamp())
	}

	retiredClient := newLifecycleTestClient()
	retiredClient.Main.Bridge.DB = db
	retiredClient.lifecycleTerminated.Store(true)
	later := past.Add(2 * fullConnectIDSCooldown)
	if _, retired := retiredClient.gateFullConnectIDSSweep(zerolog.Nop(), later); !retired {
		t.Fatal("a retired Connect was not told to stop at the sweep gate")
	}
	if stamp() != past.Format(time.RFC3339) {
		t.Fatalf("a retired Connect stamped im.last_full_connect (%q): the replacement's Connect would skip its StatusKit sweep for the whole cooldown", stamp())
	}
}

// Round-9 survivor: the keystore-loss return was untested. It must clear the
// stale login state, ask for a re-login with StateBadCredentials, close the APS
// connection LoadUserLogin built, close the epoch, and never build a client.
func TestConnectAbandonsTheEpochWhenTheKeystoreIsLost(t *testing.T) {
	closed, states, mu := installLifecycleSeams(t)
	validateKeystore = func(*rustpushgo.WrappedIdsUsers) bool { return false }
	built := false
	newRustClient = func(*rustpushgo.WrappedApsConnection, *rustpushgo.WrappedIdsUsers, *rustpushgo.WrappedIdsngmIdentity, *rustpushgo.WrappedOsConfig, **rustpushgo.WrappedTokenProvider, rustpushgo.MessageCallback, rustpushgo.UpdateUsersCallback) (*rustpushgo.Client, error) {
		built = true
		return nil, errors.New("must not be reached")
	}

	client := newLifecycleTestClient()
	client.users = &rustpushgo.WrappedIdsUsers{} // never touched: the seam answers for it
	client.UserLogin.Bridge = &bridgev2.Bridge{DB: newTestBridgeDB(t)}
	meta := client.UserLogin.Metadata.(*UserLoginMetadata)
	meta.IDSUsers, meta.IDSIdentity, meta.APSState = "users", "identity", "aps"

	client.Connect(context.Background())

	if built {
		t.Fatal("NewClient ran after the keystore check failed")
	}
	if meta.IDSUsers != "" || meta.IDSIdentity != "" || meta.APSState != "" {
		t.Fatalf("stale login state was not cleared: %q %q %q", meta.IDSUsers, meta.IDSIdentity, meta.APSState)
	}
	mu.Lock()
	defer mu.Unlock()
	if *closed != 1 {
		t.Fatalf("APS connection closed %d times, want 1: an open orphan retries Apple every <=30s for the life of the process", *closed)
	}
	if len(*states) != 1 || (*states)[0].StateEvent != status.StateBadCredentials {
		t.Fatalf("states = %v, want exactly one StateBadCredentials", *states)
	}
	if client.connectEpochActive() || client.hasOrphanedRecovery() {
		t.Fatal("the lifecycle predicates still report a live epoch or an armed loop after the keystore-loss return")
	}
}

// Round-9 survivor: the hand-back schedule is the Apple-contact bound every
// safety comment in internet_recovery.go quotes (7m, 14m, 28m, then 45m), so it
// is pinned with literals here rather than derived from the constants — a
// 2m/6m schedule passed every other test.
func TestHandBackDelayScheduleIsPinned(t *testing.T) {
	want := []time.Duration{7 * time.Minute, 14 * time.Minute, 28 * time.Minute, 45 * time.Minute, 45 * time.Minute, 45 * time.Minute}
	for i, w := range want {
		if got := handBackDelay(i + 1); got != w {
			t.Errorf("handBackDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if handBackDelay(0) != 7*time.Minute {
		t.Errorf("handBackDelay(0) = %v, want the 7m floor", handBackDelay(0))
	}
	// The cap must not be below the IDS-sweep cooldown, or the comment's
	// "at most one sweep per successful connect per cooldown" stops bounding
	// consecutive hand-backs that all connect.
	if recoveryHandBackMaxDelay < fullConnectIDSCooldown {
		t.Errorf("hand-back cap %v is below fullConnectIDSCooldown %v", recoveryHandBackMaxDelay, fullConnectIDSCooldown)
	}
}

// holdLifecycleLock simulates a Connect that is still running (Connect holds
// lifecycleMu for its whole body), so a Disconnect cannot finish. Released by the
// returned func, after which the background teardown completes harmlessly
// (c.client is nil).
func holdLifecycleLock(c *IMClient) (release func()) {
	c.lifecycleMu.Lock()
	return c.lifecycleMu.Unlock
}

// Mutations 8 and 9: both load paths retire an orphaned client and both ABORT
// when that cannot finish in time, because continuing opens a duplicate APNs
// connection on the same device token while the orphan can still push
// StateUnknownError on the shared bridge-state queue.
func TestBothLoadPathsAbortWhenThePreviousClientWillNotRetire(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	saved := loadUserLoginDisconnectTimeout
	t.Cleanup(func() { loadUserLoginDisconnectTimeout = saved })
	loadUserLoginDisconnectTimeout = 50 * time.Millisecond

	orphan := func() *IMClient {
		c := newLifecycleTestClient()
		c.stopChan = make(chan struct{})
		c.recoveryDone = make(chan struct{})
		c.disconnectForInternetRecovery() // stopChan closed, recoveryDone open
		return c
	}

	t.Run("IMConnector.LoadUserLogin", func(t *testing.T) {
		previous := orphan()
		release := holdLifecycleLock(previous)
		defer func() {
			release()
			previous.Disconnect() // wait for the background teardown to finish
		}()
		login := &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "lifecycle-test", Metadata: &UserLoginMetadata{}},
			Log:       zerolog.Nop(),
			Client:    previous,
		}
		err := previous.Main.LoadUserLogin(context.Background(), login)
		if err == nil {
			t.Fatal("LoadUserLogin proceeded while the previous client's teardown was still running")
		}
		if login.Client != previous {
			t.Fatal("an aborted load must not replace the client")
		}
	})

	t.Run("re-login closure", func(t *testing.T) {
		previous := orphan()
		release := holdLifecycleLock(previous)
		defer func() {
			release()
			previous.Disconnect()
		}()
		replacement := newLifecycleTestClient()
		login := &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "lifecycle-test"},
			Log:       zerolog.Nop(),
			Client:    previous,
		}
		err := installReLoginClient(login, replacement)
		if err == nil {
			t.Fatal("the re-login closure proceeded while the previous client's teardown was still running — the two load paths have diverged again")
		}
		if login.Client != previous || replacement.UserLogin == login {
			t.Fatal("an aborted re-login must not install the replacement")
		}
	})

	t.Run("re-login closure retires an orphan that can be retired", func(t *testing.T) {
		previous := orphan()
		replacement := newLifecycleTestClient()
		login := &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "lifecycle-test"},
			Log:       zerolog.Nop(),
			Client:    previous,
		}
		if err := installReLoginClient(login, replacement); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if previous.hasOrphanedRecovery() {
			t.Fatal("the orphan's recovery loop was not retired")
		}
		if login.Client != replacement || replacement.UserLogin != login {
			t.Fatal("the replacement was not installed")
		}
	})

	t.Run("nothing to retire", func(t *testing.T) {
		previous := newLifecycleTestClient()
		previous.connection = nil // no APS connection, no client, no epoch
		if previous.needsRetirement() {
			t.Fatal("a client that owns nothing live must not need retirement")
		}
		if err := retirePreviousClient(previous, zerolog.Nop()); err != nil {
			t.Fatalf("a client with nothing to retire must not fail the load: %v", err)
		}
	})
}

// Round-10 blocker B1: a client exactly as LoadUserLogin leaves it — APS
// connection built, no epoch, no Rust client — already has a ResourceManager
// retrying Apple on this device token, and MUST be retired before a replacement
// is installed. Every earlier predicate read Connect's progress and missed it.
func TestRetirePreviousClientRetiresAPreConnectClient(t *testing.T) {
	closed, _, mu := installLifecycleSeams(t)
	previous := newLifecycleTestClient()
	if previous.connection == nil || previous.stopChan != nil || previous.client != nil {
		t.Fatal("precondition: the fixture must look exactly like a client LoadUserLogin just built")
	}
	if !previous.needsRetirement() {
		t.Fatal("a client with a live APS connection and no epoch was not considered live")
	}
	if err := retirePreviousClient(previous, zerolog.Nop()); err != nil {
		t.Fatalf("retirement failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *closed != 1 {
		t.Fatalf("APS connection closed %d times, want 1: the replacement would otherwise share the device token with a connection still retrying Apple", *closed)
	}
	if !previous.terminationRequested() {
		t.Fatal("a retired pre-Connect client must refuse a later Connect")
	}
}

// Finding 6: every way an interactive login can end without an IMClient taking
// ownership of the APS connection must close it, or its ResourceManager retries
// Apple every <=30s for the life of the process.
func TestLoginFlowsCloseTheConnectionOnCancelAndOnError(t *testing.T) {
	closed, _, mu := installLifecycleSeams(t)
	count := func() int32 {
		mu.Lock()
		defer mu.Unlock()
		return *closed
	}
	main := &IMConnector{Bridge: &bridgev2.Bridge{}}

	t.Run("AppleIDLogin.Cancel", func(t *testing.T) {
		before := count()
		l := &AppleIDLogin{Main: main, conn: &rustpushgo.WrappedApsConnection{}}
		l.Cancel()
		if count() != before+1 {
			t.Fatal("Cancel did not close the connection")
		}
	})
	t.Run("AppleIDLogin error return", func(t *testing.T) {
		before := count()
		l := &AppleIDLogin{Main: main, conn: &rustpushgo.WrappedApsConnection{}}
		if _, err := l.SubmitUserInput(context.Background(), map[string]string{}); err == nil {
			t.Fatal("precondition: empty input must be rejected")
		}
		if count() != before+1 {
			t.Fatal("an error return did not close the connection (bridgev2 will not call Cancel after an error)")
		}
	})
	t.Run("ExternalKeyLogin.Cancel", func(t *testing.T) {
		before := count()
		l := &ExternalKeyLogin{Main: main, conn: &rustpushgo.WrappedApsConnection{}}
		l.Cancel()
		if count() != before+1 {
			t.Fatal("Cancel did not close the connection")
		}
	})
	t.Run("ExternalKeyLogin error return", func(t *testing.T) {
		before := count()
		l := &ExternalKeyLogin{Main: main, cfg: &rustpushgo.WrappedOsConfig{}, conn: &rustpushgo.WrappedApsConnection{}}
		if _, err := l.SubmitUserInput(context.Background(), map[string]string{}); err == nil {
			t.Fatal("precondition: empty input must be rejected")
		}
		if count() != before+1 {
			t.Fatal("an error return did not close the connection")
		}
	})
	t.Run("a successful step keeps the connection", func(t *testing.T) {
		before := count()
		l := &AppleIDLogin{Main: main, conn: &rustpushgo.WrappedApsConnection{}, result: &rustpushgo.IdsUsersWithIdentityRecord{}}
		step, err := l.SubmitUserInput(context.Background(), map[string]string{"device": "1"})
		if err != nil || step == nil {
			t.Fatalf("precondition: device selection should yield the passcode step, got (%v, %v)", step, err)
		}
		if count() != before {
			t.Fatal("a successful step closed the connection the next step still needs")
		}
	})
	t.Run("no connection yet is a no-op", func(t *testing.T) {
		before := count()
		(&ExternalKeyLogin{Main: main}).Cancel()
		if count() != before {
			t.Fatal("closed a connection that was never opened")
		}
	})
}

// Mutation 10: clearing the latch and draining the wake token must happen under
// one lock hold. Split, a concurrent OnConnectionEvent can set the latch after
// the clear and deposit its token before the drain, which then eats it — an
// event latched with no wake token, and the event loop asleep. The invariant
// after any interleaving of the two operations: a latched event always has its
// wake token.
func TestDropPendingConnectionEventNeverStrandsALatchedEvent(t *testing.T) {
	// 300k iterations: the losing interleaving is a sub-microsecond window
	// between the split clear and the drain, so a small sample misses it. At
	// this count the split lock was caught in every one of ten sampled runs;
	// at 20k it was caught in three of five.
	const iterations = 300000
	for i := range iterations {
		client := &IMClient{connectionEventWake: make(chan struct{}, 1)}
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			client.OnConnectionEvent(rustpushgo.ApsConnectionEventRetryFailed)
		}()
		go func() {
			defer wg.Done()
			<-start
			client.dropPendingConnectionEvent()
		}()
		close(start)
		wg.Wait()
		client.connectionEventMu.Lock()
		pending, tokens := client.connectionEventPending, len(client.connectionEventWake)
		client.connectionEventMu.Unlock()
		if pending != 0 && tokens == 0 {
			t.Fatalf("iteration %d: event latched with no wake token — the event loop would sleep on it", i)
		}
	}
}
