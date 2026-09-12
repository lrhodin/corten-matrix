use rustpush::{ResourceFailure, ResourceState};

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum APSConnectionEvent {
    Interrupted,
    RetryFailed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum APSObservedState {
    Generated,
    Generating,
    /// A generation attempt failed and the ResourceManager WILL retry
    /// (`ResourceFailure.retry_wait` is `Some`). rustpush re-publishes `Failed`
    /// on every failed attempt while backing off 1s/2s/4s/.../30s, so this is
    /// the ordinary "transport dropped, reconnecting" signal, not a give-up.
    FailedRetrying,
    /// A generation attempt failed and the ResourceManager will NOT retry
    /// (`retry_wait: None`, i.e. `PushError::DoNotRetry`). This is the only
    /// terminal failure, and it is the discriminator rustpush itself uses.
    FailedFinal,
    Closed,
}

/// Consecutive retrying failures that count as a sustained failure rather than a
/// blip. rustpush backs off 1s/2s/4s/8s/16s/30s between attempts, so when
/// `generate()` fails fast (a refused connect, no route) the fifth failure lands
/// about 15s after the first — long enough not to fire on a momentary blip,
/// short enough to stop the retry loop before it has hammered Apple for
/// minutes. When `generate()` stalls instead — a blackholed connect that only
/// ends at the ResourceManager's 300s generate timeout (rustpush aps.rs) — the
/// same five failures take about 5 x 300s plus the backoff, roughly 25 minutes.
/// That slow path is not what bounds Apple contact: the Go side's receive-wedge
/// watchdog fires after 600s without an inbound frame and classifies the link
/// itself, so a blackholed courier is handled long before this count is
/// reached. This count is the fast-failure path's signal. Because it can land
/// inside the Go side's outage-confirmation window, that side discards an event
/// latched during a window it then watched recover.
///
/// `RetryFailed` has three emission sites — `initial_event()`, the `FailedFinal`
/// arm, and this escalation — and only the escalation is live on the APS path:
/// rustpush builds `PushError::DoNotRetry` only in the IdentityManager. If a
/// future upstream made APS `generate()` return it, util.rs publishes
/// `Failed { retry_wait: None }` and then `Closed` a few statements later, and a
/// watch observer that wakes after both sees only `Closed` — this observer
/// would report nothing, and the Go receive-wedge watchdog (no frames for 600s)
/// would be what notices the resource died. Any value from 2 up passes a test
/// that derives its bound from this constant, so the tests below pin it with
/// literal five-failure traces instead.
const RETRYING_FAILURE_ESCALATION: u32 = 5;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum APSConnectionPhase {
    AwaitingFirstConnection,
    Generated,
    Reconnecting,
    /// Nothing more will come from this resource until a new `Generated`: an
    /// intentional close, or a final failure after which the ResourceManager
    /// has stopped its loop.
    Closed,
}

pub(crate) struct APSConnectionTransition {
    phase: APSConnectionPhase,
    initial_state: APSObservedState,
    /// Consecutive retrying failures in the current streak. Reset in exactly
    /// two places: a `Generated` observation (a working connection is the only
    /// thing that ends a streak) and the escalation itself (so the next streak
    /// needs a full count again rather than escalating on every later failure).
    retrying_failures: u32,
}

impl APSConnectionTransition {
    pub(crate) fn new(initial_state: APSObservedState) -> Self {
        let (phase, retrying_failures) = match initial_state {
            APSObservedState::Generated => (APSConnectionPhase::Generated, 0),
            APSObservedState::Closed => (APSConnectionPhase::Closed, 0),
            APSObservedState::Generating => (APSConnectionPhase::AwaitingFirstConnection, 0),
            // The current value IS one failed attempt: count it, exactly as the
            // Generated -> Failed edge in observe() does, so a cold start into
            // an ongoing outage (lib.rs constructs the transition from the
            // already-current watch value, after the manager loop has started)
            // escalates on the fifth failure and not the sixth.
            APSObservedState::FailedRetrying => (APSConnectionPhase::AwaitingFirstConnection, 1),
            APSObservedState::FailedFinal => (APSConnectionPhase::Closed, 0),
        };
        Self {
            phase,
            initial_state,
            retrying_failures,
        }
    }

    pub(crate) fn initial_event(&self) -> Option<APSConnectionEvent> {
        // Only a terminal failure justifies tearing down a client we just built.
        // A retrying failure means rustpush is already backing off toward a
        // reconnect, which costs nothing and needs no intervention.
        matches!(self.initial_state, APSObservedState::FailedFinal)
            .then_some(APSConnectionEvent::RetryFailed)
    }

    pub(crate) fn observe(&mut self, state: APSObservedState) -> Option<APSConnectionEvent> {
        match state {
            APSObservedState::Generated => {
                self.phase = APSConnectionPhase::Generated;
                self.retrying_failures = 0;
                None
            }
            APSObservedState::Generating => {
                if self.phase == APSConnectionPhase::Generated {
                    // No counter reset here: only the Generated arm sets that
                    // phase, and it has already zeroed the counter.
                    self.phase = APSConnectionPhase::Reconnecting;
                    Some(APSConnectionEvent::Interrupted)
                } else {
                    None
                }
            }
            APSObservedState::FailedRetrying => match self.phase {
                APSConnectionPhase::Generated => {
                    // Tokio watch values may coalesce Generated -> Generating ->
                    // Failed into Generated -> Failed for a slower observer, so
                    // this is the same departure-from-Generated edge that
                    // Generating reports: an interruption whose reconnect
                    // rustpush is already retrying on its own backoff. The
                    // failure itself still counts as the streak's first.
                    self.phase = APSConnectionPhase::Reconnecting;
                    self.retrying_failures = 1;
                    Some(APSConnectionEvent::Interrupted)
                }
                // Already reconnecting. One further attempt is a blip and must
                // not trigger a teardown, but they must still be COUNTED:
                // FailedFinal never occurs on the APS path (rustpush only builds
                // PushError::DoNotRetry in the IdentityManager), so repetition is
                // the only evidence of a sustained failure there is. Without this
                // the controller would go permanently deaf after the first blip
                // while rustpush retried Apple every 30s for the whole outage.
                APSConnectionPhase::AwaitingFirstConnection | APSConnectionPhase::Reconnecting => {
                    self.phase = APSConnectionPhase::Reconnecting;
                    self.retrying_failures = self.retrying_failures.saturating_add(1);
                    if self.retrying_failures >= RETRYING_FAILURE_ESCALATION {
                        self.retrying_failures = 0;
                        Some(APSConnectionEvent::RetryFailed)
                    } else {
                        None
                    }
                }
                APSConnectionPhase::Closed => None,
            },
            APSObservedState::FailedFinal => match self.phase {
                APSConnectionPhase::Closed => None,
                _ => {
                    // The manager has given up (util.rs breaks its loop on a
                    // final failure), so the resource is the same sink as an
                    // intentional close — which also reports a coalesced repeat
                    // of the same final failure once, not twice.
                    self.phase = APSConnectionPhase::Closed;
                    Some(APSConnectionEvent::RetryFailed)
                }
            },
            APSObservedState::Closed => {
                self.phase = APSConnectionPhase::Closed;
                None
            }
        }
    }
}

pub(crate) fn observed_aps_state(state: &ResourceState) -> APSObservedState {
    match state {
        ResourceState::Generated => APSObservedState::Generated,
        ResourceState::Generating => APSObservedState::Generating,
        // `retry_wait` is Some(seconds) while the ResourceManager intends to try
        // again and None only when it has given up (rustpush util.rs sets it
        // from `!is_final`, and checks `retry_wait: None` itself to decide a
        // resource is dead). Collapsing both into one "failed" signal turned a
        // one-second reconnect into a full client teardown.
        ResourceState::Failed(ResourceFailure { retry_wait: None, .. }) => {
            APSObservedState::FailedFinal
        }
        ResourceState::Failed(_) => APSObservedState::FailedRetrying,
        ResourceState::Closed => APSObservedState::Closed,
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use rustpush::{PushError, ResourceFailure, ResourceState};

    use super::{
        observed_aps_state, APSConnectionEvent, APSConnectionTransition, APSObservedState,
    };

    // Every escalation test below counts failures with a literal, never with
    // RETRYING_FAILURE_ESCALATION: a bound derived from the constant is
    // satisfied by any value of it from 2 up.

    fn failed(retry_wait: Option<u64>) -> ResourceState {
        ResourceState::Failed(ResourceFailure {
            retry_wait,
            error: Arc::new(PushError::ResourceGenTimeout),
        })
    }

    #[test]
    fn observed_state_maps_every_resource_state_by_its_retry_discriminator() {
        assert_eq!(
            observed_aps_state(&ResourceState::Generated),
            APSObservedState::Generated
        );
        assert_eq!(
            observed_aps_state(&ResourceState::Generating),
            APSObservedState::Generating
        );
        // A pending retry, however short, is the ordinary reconnect signal.
        assert_eq!(
            observed_aps_state(&failed(Some(1))),
            APSObservedState::FailedRetrying
        );
        assert_eq!(
            observed_aps_state(&failed(Some(0))),
            APSObservedState::FailedRetrying
        );
        // Only a manager that has given up is terminal.
        assert_eq!(
            observed_aps_state(&failed(None)),
            APSObservedState::FailedFinal
        );
        assert_eq!(
            observed_aps_state(&ResourceState::Closed),
            APSObservedState::Closed
        );
    }

    #[test]
    fn an_ordinary_retrying_failure_is_never_a_teardown_request() {
        // Through the real mapping: the first retrying failure after a working
        // connection is an interruption, not RetryFailed.
        let mut transition = APSConnectionTransition::new(observed_aps_state(&ResourceState::Generated));
        assert_eq!(
            transition.observe(observed_aps_state(&failed(Some(1)))),
            Some(APSConnectionEvent::Interrupted)
        );
        assert_eq!(transition.observe(observed_aps_state(&failed(Some(2)))), None);
    }

    #[test]
    fn generated_departure_emits_one_interruption_then_failed_retry() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        assert_eq!(
            transition.observe(APSObservedState::FailedFinal),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn generated_to_failed_coalescing_reports_an_interruption_and_counts_the_failure() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        // Coalesced Generated -> Generating -> Failed(retrying). rustpush is
        // backing off toward its own reconnect, so this is the ordinary
        // interruption edge, not a reason to tear the client down.
        assert_eq!(
            transition.observe(APSObservedState::FailedRetrying),
            Some(APSConnectionEvent::Interrupted)
        );
        // But it was a failed attempt, and the streak counts it: the ordinary
        // upstream interleaving (util.rs republishes Generating before every
        // retry) must escalate on the FIFTH failure, this one included.
        for _ in 0..3 {
            assert_eq!(transition.observe(APSObservedState::Generating), None);
            assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        }
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        assert_eq!(
            transition.observe(APSObservedState::FailedRetrying),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn escalation_lands_on_exactly_the_fifth_failure_after_an_interruption() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );

        // rustpush re-publishes Failed on every attempt of its backoff. Four
        // are a blip and must not request a teardown.
        for _ in 0..4 {
            assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
            assert_eq!(transition.observe(APSObservedState::Generating), None);
        }
        // The fifth is sustained: FailedFinal never occurs on the APS path, so
        // without this the controller goes permanently deaf.
        assert_eq!(
            transition.observe(APSObservedState::FailedRetrying),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn a_cold_start_into_an_outage_escalates_on_the_fifth_failure_too() {
        // lib.rs constructs the transition from the already-current watch
        // value, so a bridge started during an outage begins here. The initial
        // failure is not reported (rustpush is retrying on its own) but it is
        // the streak's first attempt.
        let mut transition = APSConnectionTransition::new(APSObservedState::FailedRetrying);
        assert_eq!(transition.initial_event(), None);

        for _ in 0..3 {
            assert_eq!(transition.observe(APSObservedState::Generating), None);
            assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        }
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        assert_eq!(
            transition.observe(APSObservedState::FailedRetrying),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn ten_consecutive_failures_escalate_exactly_twice() {
        // After an escalation the streak starts over, so a resource that keeps
        // failing re-escalates every five failures — not on every later one.
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        transition.observe(APSObservedState::Generating);

        let events: Vec<Option<APSConnectionEvent>> = (0..10)
            .map(|_| transition.observe(APSObservedState::FailedRetrying))
            .collect();
        let escalations: Vec<usize> = events
            .iter()
            .enumerate()
            .filter(|(_, e)| **e == Some(APSConnectionEvent::RetryFailed))
            .map(|(i, _)| i + 1)
            .collect();
        assert_eq!(escalations, vec![5, 10]);
    }

    #[test]
    fn a_recovered_connection_resets_the_escalation_counter() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        transition.observe(APSObservedState::Generating);
        for _ in 0..4 {
            assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        }
        // Back to healthy: the four earlier failures must not carry over. The
        // next streak needs five of its own, and escalates on its fifth.
        assert_eq!(transition.observe(APSObservedState::Generated), None);
        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
        for _ in 0..4 {
            assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        }
        assert_eq!(
            transition.observe(APSObservedState::FailedRetrying),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn only_a_terminal_failure_requests_conservative_recovery() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        transition.observe(APSObservedState::Generating);
        assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        assert_eq!(
            transition.observe(APSObservedState::FailedFinal),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn a_final_failure_is_reported_once_and_then_the_resource_is_a_sink() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        transition.observe(APSObservedState::Generating);
        assert_eq!(
            transition.observe(APSObservedState::FailedFinal),
            Some(APSConnectionEvent::RetryFailed)
        );
        // A coalesced repeat, or a stray retrying value, after the manager has
        // stopped: nothing further to report.
        assert_eq!(transition.observe(APSObservedState::FailedFinal), None);
        assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        // A new Generated re-arms everything.
        assert_eq!(transition.observe(APSObservedState::Generated), None);
        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
    }

    #[test]
    fn a_cold_start_while_generating_reports_nothing_until_a_real_departure() {
        // aps.rs passes ok = None to the ResourceManager when the first
        // generate() fails, and util.rs then seeds the watch at Generating, so
        // a bridge started into an outage begins here. Generating republished
        // before each retry is not an interruption — nothing was ever up.
        let mut transition = APSConnectionTransition::new(APSObservedState::Generating);
        assert_eq!(transition.initial_event(), None);
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        // The first working connection arms the interruption edge.
        assert_eq!(transition.observe(APSObservedState::Generated), None);
        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
    }

    #[test]
    fn initial_retrying_failure_is_not_reported() {
        let transition = APSConnectionTransition::new(APSObservedState::FailedRetrying);
        assert_eq!(transition.initial_event(), None);
    }

    #[test]
    fn later_generated_state_rearms_interruption() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);
        transition.observe(APSObservedState::Generating);
        transition.observe(APSObservedState::Generated);

        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
    }

    #[test]
    fn initial_failed_connection_is_reported_as_retry_failure() {
        let transition = APSConnectionTransition::new(APSObservedState::FailedFinal);

        assert_eq!(
            transition.initial_event(),
            Some(APSConnectionEvent::RetryFailed)
        );
        // And only once: the same value observed again is not a second request.
        let mut transition = transition;
        assert_eq!(transition.observe(APSObservedState::FailedFinal), None);
    }

    #[test]
    fn intentional_close_never_requests_recovery() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        assert_eq!(transition.observe(APSObservedState::Closed), None);
        assert_eq!(transition.observe(APSObservedState::FailedRetrying), None);
    }
}
