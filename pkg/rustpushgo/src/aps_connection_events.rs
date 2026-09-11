use rustpush::ResourceState;

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum APSConnectionEvent {
    Interrupted,
    RetryFailed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum APSObservedState {
    Generated,
    Generating,
    Failed,
    Closed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum APSConnectionPhase {
    AwaitingFirstConnection,
    Generated,
    Reconnecting,
    Closed,
}

pub(crate) struct APSConnectionTransition {
    phase: APSConnectionPhase,
    initial_state: APSObservedState,
}

impl APSConnectionTransition {
    pub(crate) fn new(initial_state: APSObservedState) -> Self {
        let phase = match initial_state {
            APSObservedState::Generated => APSConnectionPhase::Generated,
            APSObservedState::Closed => APSConnectionPhase::Closed,
            APSObservedState::Generating | APSObservedState::Failed => {
                APSConnectionPhase::AwaitingFirstConnection
            }
        };
        Self {
            phase,
            initial_state,
        }
    }

    pub(crate) fn initial_event(&self) -> Option<APSConnectionEvent> {
        matches!(self.initial_state, APSObservedState::Failed)
            .then_some(APSConnectionEvent::RetryFailed)
    }

    pub(crate) fn observe(&mut self, state: APSObservedState) -> Option<APSConnectionEvent> {
        match state {
            APSObservedState::Generated => {
                self.phase = APSConnectionPhase::Generated;
                None
            }
            APSObservedState::Generating => {
                if self.phase == APSConnectionPhase::Generated {
                    self.phase = APSConnectionPhase::Reconnecting;
                    Some(APSConnectionEvent::Interrupted)
                } else {
                    None
                }
            }
            APSObservedState::Failed => match self.phase {
                APSConnectionPhase::Generated => {
                    // Tokio watch values may coalesce Generated -> Generating ->
                    // Failed into Generated -> Failed for a slower observer. At
                    // this point regeneration has already failed, so report the
                    // conservative event rather than allowing another Apple try.
                    self.phase = APSConnectionPhase::Reconnecting;
                    Some(APSConnectionEvent::RetryFailed)
                }
                APSConnectionPhase::AwaitingFirstConnection | APSConnectionPhase::Reconnecting => {
                    self.phase = APSConnectionPhase::Reconnecting;
                    Some(APSConnectionEvent::RetryFailed)
                }
                APSConnectionPhase::Closed => None,
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
        ResourceState::Failed(_) => APSObservedState::Failed,
        ResourceState::Closed => APSObservedState::Closed,
    }
}

#[cfg(test)]
mod tests {
    use super::{APSConnectionEvent, APSConnectionTransition, APSObservedState};

    #[test]
    fn generated_departure_emits_one_interruption_then_failed_retry() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        assert_eq!(
            transition.observe(APSObservedState::Generating),
            Some(APSConnectionEvent::Interrupted)
        );
        assert_eq!(transition.observe(APSObservedState::Generating), None);
        assert_eq!(
            transition.observe(APSObservedState::Failed),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn generated_to_failed_coalescing_reports_failed_regeneration() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        assert_eq!(
            transition.observe(APSObservedState::Failed),
            Some(APSConnectionEvent::RetryFailed)
        );
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
        let transition = APSConnectionTransition::new(APSObservedState::Failed);

        assert_eq!(
            transition.initial_event(),
            Some(APSConnectionEvent::RetryFailed)
        );
    }

    #[test]
    fn intentional_close_never_requests_recovery() {
        let mut transition = APSConnectionTransition::new(APSObservedState::Generated);

        assert_eq!(transition.observe(APSObservedState::Closed), None);
    }
}
