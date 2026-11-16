pub mod dispatch;
mod handles;
mod instance;
pub mod picker;
mod picker_cancel;
mod picker_events;
mod pipe;
mod provenance;
mod security;
mod server;
pub mod share_target;
#[cfg(test)]
#[path = "pipe_tests.rs"]
mod tests;
pub mod window;

pub use instance::{InstanceOwner, Scope};
pub use server::Server;

/// # Errors
/// Fails if this executable has not acquired the registered package identity.
pub fn identity_check() -> anyhow::Result<()> {
    let _apartment = handles::Apartment::new(windows::Win32::System::WinRT::RO_INIT_MULTITHREADED)?;
    let package = windows::ApplicationModel::Package::Current()?;
    tracing::info!(event = "activation.package_identity", name = %package.Id()?.Name()?);
    Ok(())
}

pub fn show_failure(message: &str) {
    use windows::{
        Win32::UI::WindowsAndMessaging::{MB_ICONERROR, MB_OK, MessageBoxW},
        core::PCWSTR,
    };
    let message = handles::wide(message);
    let title = handles::wide("WindShare");
    // SAFETY: both strings are terminated and remain live throughout the modal call.
    unsafe {
        MessageBoxW(
            None,
            PCWSTR(message.as_ptr()),
            PCWSTR(title.as_ptr()),
            MB_OK | MB_ICONERROR,
        );
    }
}
