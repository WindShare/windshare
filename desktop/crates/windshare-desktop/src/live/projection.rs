use crate::{
    model::{Share, ShareState},
    sharing::ShareLink,
};
use windshare_host::{Operation, OperationState, ShareState as HostState};

pub fn update_share(share: &mut Share, operation: &Operation) {
    share.error = operation.error().map(str::to_owned);
    share.state = match &operation.state {
        OperationState::Submitting => ShareState::Preparing,
        OperationState::Rejected(_) => ShareState::Failed,
        OperationState::Accepted(task) => match task.state {
            HostState::Preparing => ShareState::Preparing,
            HostState::Sharing => ShareState::Live { viewers: None },
            HostState::Stopping => ShareState::Stopping,
            HostState::Finished => match task.result.as_ref().map(|result| result.outcome.as_str())
            {
                Some("stopped") => ShareState::Stopped,
                Some("cancelled") => ShareState::Cancelled,
                _ => ShareState::Failed,
            },
        },
    };
    if let Some(task) = operation.share() {
        if !task.url.is_empty()
            && share
                .link
                .as_ref()
                .is_none_or(|link| link.url().as_ref() != task.url)
        {
            share.link = Some(ShareLink::new(task.url.clone()));
        }
        share.relay_reconnecting = task.state == HostState::Sharing
            && task
                .relay
                .as_ref()
                .is_some_and(|relay| relay.payload.available == 0);
    }
}
