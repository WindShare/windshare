use crate::{
    model::{Share, ShareAge, ShareContents, ShareState},
    sharing::SharePublication,
};
use std::time::{Duration, SystemTime};
use windshare_host::{Operation, OperationState, Outcome, SelectedRoot, ShareState as HostState};
use windshare_ui::Icon;

pub fn update_share(share: &mut Share, operation: &Operation) {
    let error = operation.error().map(str::to_owned);
    match &operation.state {
        OperationState::Submitting => {
            share.state = ShareState::Preparing { error: None };
        }
        OperationState::Rejected(_) => {
            share.state = ShareState::Failed {
                error,
                authorization: None,
            };
        }
        OperationState::Accepted(task) => {
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
                        share.size = Some(byte_size(*file_size));
                    }
                    Some(SelectedRoot::Directory { name }) => {
                        share.name.clone_from(name);
                        share.symbol = Icon::Folder;
                        share.size = None;
                    }
                    None => {}
                }
            }
            match &task.state {
                HostState::AwaitingAuthorization { authorization } => {
                    share.state = ShareState::AwaitingAuthorization(authorization.clone());
                }
                HostState::AwaitingConfiguration { blocking, .. } => {
                    share.state = ShareState::AwaitingConfiguration {
                        blocking: *blocking,
                        error,
                    };
                }
                HostState::AwaitingNetwork { blocking, .. } => {
                    share.state = ShareState::AwaitingNetwork {
                        blocking: *blocking,
                        error,
                    };
                }
                HostState::Admitting | HostState::Preparing { .. } => {
                    share.state = ShareState::Preparing { error };
                }
                HostState::Sharing {
                    url,
                    backup_url,
                    relay,
                    ..
                } => {
                    let relay_status = relay
                        .as_ref()
                        .map(windshare_host::RelayAvailability::status);
                    // Preserve the existing SharePublication so steady-state snapshots
                    // do not re-encode the cached QR image.
                    if let ShareState::Live {
                        publication,
                        relay_status: current,
                    } = &mut share.state
                        && publication.matches(url, backup_url.as_deref())
                    {
                        *current = relay_status;
                    } else {
                        share.state = ShareState::Live {
                            publication: SharePublication::new(
                                url.clone(),
                                backup_url.clone().map(Into::into),
                            ),
                            relay_status,
                        };
                    }
                }
                HostState::Stopping { .. } => {
                    share.state = ShareState::Stopping;
                }
                HostState::Finished {
                    result,
                    authorization,
                    ..
                } => {
                    share.state = match result.outcome {
                        Outcome::Stopped => ShareState::Stopped { error },
                        Outcome::Cancelled => ShareState::Cancelled { error },
                        Outcome::Failed => ShareState::Failed {
                            error,
                            authorization: authorization.clone(),
                        },
                    };
                }
            }
        }
    }
}

pub fn byte_size(bytes: u64) -> String {
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
