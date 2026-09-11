// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"runtime"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

const (
	internetRecoveryPollInterval = 10 * time.Second
	internetRecoveryStablePeriod = 60 * time.Second
	internetReconnectRetryMargin = 60 * time.Second
)

// runAPSConnectionEventLoop performs no periodic network activity. rustpushgo
// emits an event only when the previously usable APS resource leaves Generated
// or when a regeneration fails.
func (c *IMClient) runAPSConnectionEventLoop(stop <-chan struct{}, log zerolog.Logger) {
	for {
		select {
		case <-stop:
			return
		case <-c.connectionEventWake:
			event, ok := c.takeConnectionEvent()
			if !ok {
				continue
			}
			log.Info().
				Str("platform", runtime.GOOS).
				Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
				Uint("aps_event", uint(event)).
				Msg("APS connection event received; checking public Internet before deciding how to reconnect")
			switch event {
			case rustpushgo.ApsConnectionEventInterrupted:
				publicInternetUp := runLoggedInternetProbe(context.Background(), log, "aps_interruption_classification")
				if channelClosed(stop) {
					return
				}
				if publicInternetUp {
					log.Info().Msg("APS transport interrupted while public Internet remains reachable; allowing immediate transport reconnect")
					continue
				}
				log.Warn().Msg("APS transport interrupted and both public Internet probes failed; stopping Apple retries")
				c.runPublicOnlyInternetRecovery(log, false)
				return

			case rustpushgo.ApsConnectionEventRetryFailed:
				publicInternetUp := runLoggedInternetProbe(context.Background(), log, "aps_retry_failure_classification")
				if channelClosed(stop) {
					return
				}
				if publicInternetUp {
					log.Warn().Msg("APS reconnect failed while public Internet is reachable; stopping retries before conservative recovery")
				} else {
					log.Warn().Msg("APS reconnect failed and both public Internet probes failed; stopping Apple retries")
				}
				c.runPublicOnlyInternetRecovery(log, publicInternetUp)
				return
			}
		}
	}
}

// runPublicOnlyInternetRecovery deliberately outlives this IMClient's stopChan.
// It tears the client down completely, stopping APS and every Apple-facing
// recurring worker. The bridge context then owns the public-only recovery loop.
func (c *IMClient) runPublicOnlyInternetRecovery(log zerolog.Logger, appleSpecificFailure bool) {
	main := c.Main
	bridgeState := c.recoveryBridgeState
	recoveryDone := c.recoveryDone
	c.internetRecoveryMu.Lock()
	defer c.internetRecoveryMu.Unlock()

	ctx := main.Bridge.BackgroundCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if main.Bridge.IsStopping() || bridgeState == nil || recoveryDone == nil || internetRecoveryCancelled(ctx, recoveryDone) {
		return
	}

	// Do not consume bridgev2's UserLogin.disconnectOnce here. bridgev2 needs
	// that guard when StateUnknownError performs the eventual safe recreation.
	c.disconnectForInternetRecovery()
	if main.Bridge.IsStopping() || internetRecoveryCancelled(ctx, recoveryDone) {
		return
	}
	message := "Internet connection lost; waiting to reconnect to iMessage"
	if appleSpecificFailure {
		message = "Apple connection failed; waiting before reconnecting to iMessage"
	}
	if !sendInternetRecoveryState(main.Bridge, bridgeState, status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "im-internet-offline",
		Message:    message,
	}) {
		return
	}
	log.Warn().
		Str("platform", runtime.GOOS).
		Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
		Bool("apple_specific_failure", appleSpecificFailure).
		Dur("required_stability", internetRecoveryStablePeriod).
		Msg("APNs recovery entered public-only mode: iMessage client teardown completed; the recovery watcher will use only public probes until Internet stability is established")

	var stable internetprobe.Stability
	stabilizing := false
	reconnectRequested := false
	var reconnectRequestedAt time.Time
	reconnectAttempt := 0
	for {
		if main.Bridge.IsStopping() || internetRecoveryCancelled(ctx, recoveryDone) {
			return
		}

		reachable := runLoggedInternetProbe(ctx, log, "public_only_recovery")
		if main.Bridge.IsStopping() || internetRecoveryCancelled(ctx, recoveryDone) {
			return
		}
		now := time.Now()
		if !reachable {
			if stabilizing {
				log.Warn().Msg("Public Internet probe failed during stabilization; resetting the 60-second recovery window")
			} else {
				log.Info().Dur("next_test_in", internetRecoveryPollInterval).
					Msg("Public Internet test failed: neither public target replied; remaining in Apple-free recovery mode")
			}
			stabilizing = false
			stable.Reset()
			if reconnectRequested {
				if !sendInternetRecoveryState(main.Bridge, bridgeState, status.BridgeState{
					StateEvent: status.StateTransientDisconnect,
					Error:      "im-internet-offline",
					Message:    "Internet became unstable; reconnect delay reset",
				}) {
					return
				}
				reconnectRequested = false
				reconnectRequestedAt = time.Time{}
			}
		} else {
			ready := stable.Observe(now, true, internetRecoveryStablePeriod)
			if !stabilizing {
				stabilizing = true
				log.Info().Dur("required_stability", internetRecoveryStablePeriod).
					Msg("Public Internet probe succeeded; starting the continuous recovery-stability window")
			}
			if ready {
				retryDelay := internetRecoveryStateRetryDelay(main.Bridge.Config.UnknownErrorAutoReconnect)
				if !reconnectRequested || now.Sub(reconnectRequestedAt) >= retryDelay {
					// Final public preflight immediately before authorizing a single
					// bridgev2-owned reconstruction.
					finalPreflightOK := runLoggedInternetProbe(ctx, log, "final_reconnect_preflight")
					if main.Bridge.IsStopping() || internetRecoveryCancelled(ctx, recoveryDone) {
						return
					}
					if !finalPreflightOK {
						log.Warn().Msg("Final public Internet preflight failed; resetting the recovery-stability window without contacting Apple")
						stabilizing = false
						stable.Reset()
						reconnectRequested = false
						reconnectRequestedAt = time.Time{}
					} else {
						reconnectAttempt++
						log.Info().Dur("stable_for", internetRecoveryStablePeriod).
							Int("request_attempt", reconnectAttempt).
							Msg("Public Internet remained stable and final preflight passed; authorizing one bridgev2 iMessage client rebuild")
						if !sendInternetRecoveryState(main.Bridge, bridgeState, status.BridgeState{
							StateEvent: status.StateUnknownError,
							Error:      "im-internet-recovered",
							Message:    "Internet recovered; reconnecting to iMessage",
							Info:       map[string]interface{}{"recovery_request": reconnectAttempt},
						}) {
							return
						}
						reconnectRequested = true
						reconnectRequestedAt = now
					}
				}
			}
		}

		if !waitForInternetRecoveryContext(ctx, recoveryDone, internetRecoveryPollInterval) {
			return
		}
	}
}

func runLoggedInternetProbe(ctx context.Context, log zerolog.Logger, phase string) bool {
	log.Info().
		Str("platform", runtime.GOOS).
		Str("probe_phase", phase).
		Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
		Msg("Testing public Internet connectivity without contacting Apple")
	result := internetprobe.Probe(ctx)
	logInternetProbeResult(log, phase, result)
	return result.Reachable()
}

func logInternetProbeResult(log zerolog.Logger, phase string, result internetprobe.Result) {
	log.Info().
		Str("platform", runtime.GOOS).
		Str("probe_phase", phase).
		Str("cloudflare_target", internetprobe.CloudflareTarget).
		Bool("cloudflare_reachable", result.Cloudflare).
		Str("google_target", internetprobe.GoogleTarget).
		Bool("google_reachable", result.Google).
		Bool("internet_reachable", result.Reachable()).
		Msg("Public Internet connectivity test completed")
}

func sendInternetRecoveryState(bridge *bridgev2.Bridge, bridgeState *bridgev2.BridgeStateQueue, state status.BridgeState) (sent bool) {
	if bridge == nil || bridge.IsStopping() || bridgeState == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()
	bridgeState.Send(state)
	return true
}

func internetRecoveryStateRetryDelay(autoReconnect time.Duration) time.Duration {
	if autoReconnect < time.Minute {
		autoReconnect = time.Minute
	}
	// bridgev2 jitters by up to +20%. Retry the reconstruction signal only
	// after the complete window plus a margin, never the Apple connection itself.
	return autoReconnect + autoReconnect/5 + internetReconnectRetryMargin
}

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func internetRecoveryCancelled(ctx context.Context, recoveryDone <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-recoveryDone:
		return true
	default:
		return false
	}
}

func waitForInternetRecoveryContext(ctx context.Context, recoveryDone <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-recoveryDone:
		return false
	case <-timer.C:
		return true
	}
}
