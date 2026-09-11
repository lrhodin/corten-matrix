package connector

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

func TestInternetProbeResultLogIncludesPhaseTargetsAndOutcomes(t *testing.T) {
	var output bytes.Buffer
	logger := zerolog.New(&output)
	logInternetProbeResult(logger, "outage_recovery", internetprobe.Result{Google: true})

	line := output.String()
	for _, want := range []string{
		`"probe_phase":"outage_recovery"`,
		`"cloudflare_target":"1.1.1.1"`,
		`"cloudflare_reachable":false`,
		`"google_target":"8.8.8.8"`,
		`"google_reachable":true`,
		`"internet_reachable":true`,
		`Public Internet connectivity test completed`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("probe log %q does not contain %q", line, want)
		}
	}
}

func TestConnectionEventLatchPreservesRetryFailurePriority(t *testing.T) {
	client := &IMClient{connectionEventWake: make(chan struct{}, 1)}
	for range 100 {
		client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
	}
	client.OnConnectionEvent(rustpushgo.ApsConnectionEventRetryFailed)

	select {
	case <-client.connectionEventWake:
	default:
		t.Fatal("connection event did not wake recovery loop")
	}
	event, ok := client.takeConnectionEvent()
	if !ok || event != rustpushgo.ApsConnectionEventRetryFailed {
		t.Fatalf("pending event = (%v, %v), want RetryFailed", event, ok)
	}
}

func TestConnectCannotBeginAfterLifecycleDisconnect(t *testing.T) {
	client := &IMClient{}
	client.Disconnect()

	if client.beginConnect() {
		client.endConnect()
		t.Fatal("Connect began after normal lifecycle teardown completed")
	}
}

func TestDisconnectWaitsForConnectLifecycle(t *testing.T) {
	client := &IMClient{
		stopChan:     make(chan struct{}),
		recoveryDone: make(chan struct{}),
	}
	client.lifecycleMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		client.Disconnect()
		close(done)
	}()
	<-started

	select {
	case <-done:
		client.lifecycleMu.Unlock()
		t.Fatal("Disconnect completed while Connect lifecycle lock was held")
	default:
	}
	client.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not resume after Connect lifecycle lock was released")
	}
}

func TestDisconnectKeepsClosedStopChannelObservable(t *testing.T) {
	stop := make(chan struct{})
	recoveryDone := make(chan struct{})
	client := &IMClient{
		stopChan:     stop,
		recoveryDone: recoveryDone,
	}

	client.Disconnect()
	client.Disconnect() // must remain idempotent

	if client.stopChan == nil {
		t.Fatal("Disconnect replaced the closed stop channel with nil")
	}
	select {
	case <-stop:
	default:
		t.Fatal("Disconnect did not close the worker stop channel")
	}
	select {
	case <-recoveryDone:
	default:
		t.Fatal("lifecycle Disconnect did not cancel Internet recovery")
	}
}

func TestRecoveryDisconnectLeavesBridgeRecoveryAlive(t *testing.T) {
	recoveryDone := make(chan struct{})
	client := &IMClient{
		stopChan:     make(chan struct{}),
		recoveryDone: recoveryDone,
	}

	client.disconnectForInternetRecovery()

	select {
	case <-recoveryDone:
		t.Fatal("recovery teardown cancelled its own bridge-owned recovery loop")
	default:
	}
	select {
	case <-client.stopChan:
	default:
		t.Fatal("recovery teardown did not stop client-epoch workers")
	}
}

func TestInternetRecoveryStateRetryDelayOutlastsBridgeJitter(t *testing.T) {
	const configured = 5 * time.Minute
	want := configured + configured/5 + internetReconnectRetryMargin
	if got := internetRecoveryStateRetryDelay(configured); got != want {
		t.Fatalf("retry delay = %s, want %s", got, want)
	}
}
