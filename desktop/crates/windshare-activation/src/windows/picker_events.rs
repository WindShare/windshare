// windows::implement generates these patterns in COM vtables.
#![allow(clippy::ref_as_ptr, clippy::inline_always)]
use super::picker::State;
use std::sync::{Arc, Mutex};
use windows::{
    Win32::{
        Foundation::ERROR_CANCELLED,
        UI::Shell::{
            FDE_OVERWRITE_RESPONSE, FDE_SHAREVIOLATION_RESPONSE, FDEOR_DEFAULT, FDESVR_DEFAULT,
            IFileDialog, IFileDialogEvents, IFileDialogEvents_Impl, IShellItem,
        },
    },
    core::{Ref, implement},
};

#[implement(IFileDialogEvents)]
pub(super) struct Events(pub Arc<Mutex<State>>);

impl Events {
    fn cancel_if_requested(&self, dialog: &Ref<IFileDialog>) -> windows::core::Result<()> {
        let state = self.0.lock().expect("picker state");
        let cancelled = state.cancelled;
        let id = state.id;
        drop(state);
        if cancelled && let Some(dialog) = dialog.as_ref() {
            tracing::info!(event = "activation.picker_cancel_during_initialization", picker_id = %id);
            // Cancellation can arrive while Show initializes its native window.
            // SAFETY: this live dialog calls us on its STA, the supported context
            // for IFileDialog::Close.
            unsafe {
                dialog.Close(ERROR_CANCELLED.to_hresult())?;
            }
        }
        Ok(())
    }
}

impl IFileDialogEvents_Impl for Events_Impl {
    fn OnFileOk(&self, dialog: Ref<IFileDialog>) -> windows::core::Result<()> {
        self.cancel_if_requested(&dialog)
    }
    fn OnFolderChanging(
        &self,
        dialog: Ref<IFileDialog>,
        _: Ref<IShellItem>,
    ) -> windows::core::Result<()> {
        self.cancel_if_requested(&dialog)
    }
    fn OnFolderChange(&self, dialog: Ref<IFileDialog>) -> windows::core::Result<()> {
        self.cancel_if_requested(&dialog)
    }
    fn OnSelectionChange(&self, dialog: Ref<IFileDialog>) -> windows::core::Result<()> {
        self.cancel_if_requested(&dialog)
    }
    fn OnTypeChange(&self, dialog: Ref<IFileDialog>) -> windows::core::Result<()> {
        self.cancel_if_requested(&dialog)
    }
    fn OnShareViolation(
        &self,
        _: Ref<IFileDialog>,
        _: Ref<IShellItem>,
    ) -> windows::core::Result<FDE_SHAREVIOLATION_RESPONSE> {
        Ok(FDESVR_DEFAULT)
    }
    fn OnOverwrite(
        &self,
        _: Ref<IFileDialog>,
        _: Ref<IShellItem>,
    ) -> windows::core::Result<FDE_OVERWRITE_RESPONSE> {
        Ok(FDEOR_DEFAULT)
    }
}
