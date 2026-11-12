use crate::{
    model::{Share, ShareAge, ShareContents, ShareState},
    sharing::ShareLink,
};
use std::time::{Duration, SystemTime};
use windshare_host::{Operation, OperationState, Outcome, SelectedRoot, ShareState as HostState};
use windshare_ui::Icon;

pub fn update_share(share: &mut Share, operation: &Operation) {
    share.error = operation.error().map(str::to_owned);
    share.state = match &operation.state {
        OperationState::Submitting => ShareState::Preparing,
        OperationState::Rejected(_) => ShareState::Failed,
        OperationState::Accepted(task) => match task.state {
            HostState::AwaitingConfiguration => ShareState::AwaitingConfiguration,
            HostState::Admitting | HostState::Preparing => ShareState::Preparing,
            HostState::Sharing => ShareState::Live { viewers: None },
            HostState::Stopping => ShareState::Stopping,
            HostState::Finished => match task.result.as_ref().map(|result| result.outcome) {
                Some(Outcome::Stopped) => ShareState::Stopped,
                Some(Outcome::Cancelled) => ShareState::Cancelled,
                _ => ShareState::Failed,
            },
        },
    };
    if let Some(task) = operation.share() {
        if let Some(created) =
            SystemTime::UNIX_EPOCH.checked_add(Duration::from_millis(task.created_at_ms))
        {
            share.age = ShareAge::CreatedAt(created);
        }
        if let Some(selection) = &task.selection {
            share.contents =
                ShareContents::Selected(usize::try_from(selection.count).unwrap_or(usize::MAX));
            match &selection.root {
                Some(SelectedRoot::File { name, file_size }) => {
                    share.name.clone_from(name);
                    share.symbol = Icon::File;
                    share.size = Some(file_size_label(*file_size));
                }
                Some(SelectedRoot::Directory { name }) => {
                    share.name.clone_from(name);
                    share.symbol = Icon::Folder;
                    share.size = None;
                }
                None => {}
            }
        }
        if task.url.is_empty() {
            share.link = None;
        }
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

fn file_size_label(bytes: u64) -> String {
    const KIB: u64 = 1024;
    const UNITS: [(u64, &str); 4] = [
        (KIB.pow(4), "TiB"),
        (KIB.pow(3), "GiB"),
        (KIB.pow(2), "MiB"),
        (KIB, "KiB"),
    ];
    for (unit, label) in UNITS {
        if bytes >= unit {
            let tenths = u128::from(bytes) * 10 / u128::from(unit);
            return format!("{}.{:01} {label}", tenths / 10, tenths % 10);
        }
    }
    format!("{bytes} B")
}
