use super::{
    Scope, dispatch,
    provenance::{Provenance, SAVE_LOCALLY},
};
use crate::{Activation, EntrySource, MAX_SELECTED_ROOTS, ShareIntent};
use std::time::{Duration, Instant};
use windows::{
    ApplicationModel::{
        Activation::{ActivationKind, IShareTargetActivatedEventArgs},
        AppInstance,
        DataTransfer::{ShareTarget::ShareOperation, StandardDataFormats},
        Package,
    },
    Win32::System::WinRT::RO_INIT_MULTITHREADED,
    core::{Interface, RuntimeType},
};
use windows_future::{AsyncStatus, IAsyncOperation};

const ACQUISITION_TIMEOUT: Duration = Duration::from_secs(30);
const ACQUISITION_POLL: Duration = Duration::from_millis(20);

/// Owns Windows share objects on a dedicated MTA through acquisition and handoff.
/// # Errors
/// Returns acquisition or application acceptance errors after notifying Windows.
pub fn handle_activation() -> anyhow::Result<bool> {
    std::thread::Builder::new()
        .name("windshare-share-target".into())
        .spawn(handle_on_mta)?
        .join()
        .map_err(|_| anyhow::anyhow!("Windows share acquisition failed"))?
}

fn handle_on_mta() -> anyhow::Result<bool> {
    let _apartment = super::handles::Apartment::new(RO_INIT_MULTITHREADED)?;
    tracing::info!(event = "activation.inspecting_system_arguments");
    if Package::Current().is_err() {
        return Ok(false);
    }
    let Ok(arguments) = AppInstance::GetActivatedEventArgs() else {
        return Ok(false);
    };
    let kind = arguments.Kind()?;
    tracing::info!(event = "activation.system_arguments", kind = kind.0);
    if kind != ActivationKind::ShareTarget {
        return Ok(false);
    }
    let operation = arguments
        .cast::<IShareTargetActivatedEventArgs>()?
        .ShareOperation()?;
    operation.ReportStarted()?;
    let result = acquire_and_handoff(&operation);
    if let Err(error) = &result {
        tracing::error!(event = "activation.system_share_failed", %error);
        let _ = operation.ReportError(&format!("{error:#}").into());
    }
    result.map(|()| true)
}

fn acquire_and_handoff(operation: &ShareOperation) -> anyhow::Result<()> {
    let started = Instant::now();
    let deadline = started + ACQUISITION_TIMEOUT;
    let data = operation.Data()?;
    anyhow::ensure!(
        data.Contains(&StandardDataFormats::StorageItems()?)?,
        "{SAVE_LOCALLY}"
    );
    let items = wait(&data.GetStorageItemsAsync()?, deadline)?;
    let count = items.Size()?;
    anyhow::ensure!(
        count > 0 && count as usize <= MAX_SELECTED_ROOTS,
        "Select between 1 and {MAX_SELECTED_ROOTS} local files or folders."
    );
    let provenance = Provenance::new();
    let mut roots = Vec::with_capacity(count as usize);
    for index in 0..count {
        roots.push(provenance.acquire(&items.GetAt(index)?, deadline)?);
    }
    let intent = ShareIntent::new(EntrySource::ShareTarget, roots);
    intent.validate().map_err(anyhow::Error::msg)?;
    // Every source is independently owned by the user's filesystem. No stream,
    // broker lease, temporary copy, or source-process object remains necessary.
    operation.ReportDataRetrieved()?;
    tracing::info!(event = "activation.system_data_retrieved", intent_id = %intent.id, count,
        elapsed_ms = started.elapsed().as_millis());
    dispatch::deliver(
        &Scope::current()?,
        &Activation::ShareSelection(intent.clone()),
        &std::env::current_exe()?,
    )?;
    operation.ReportCompleted()?;
    tracing::info!(event = "activation.system_share_completed", intent_id = %intent.id,
        elapsed_ms = started.elapsed().as_millis());
    Ok(())
}

pub(super) fn wait<T: RuntimeType + 'static>(
    operation: &IAsyncOperation<T>,
    deadline: Instant,
) -> anyhow::Result<T> {
    while operation.Status()? == AsyncStatus::Started {
        if Instant::now() >= deadline {
            let _ = operation.Cancel();
            anyhow::bail!(
                "The source application did not provide its files in time. {SAVE_LOCALLY}"
            );
        }
        std::thread::sleep(ACQUISITION_POLL);
    }
    Ok(operation.GetResults()?)
}
