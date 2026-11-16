// windows::implement generates these patterns in COM vtables.
#![allow(clippy::ref_as_ptr, clippy::inline_always)]

use std::{ffi::OsString, os::windows::ffi::OsStringExt, path::PathBuf};
use windows::{
    Win32::{
        Foundation::{E_NOTIMPL, HMODULE},
        Globalization::GetUserDefaultUILanguage,
        System::{
            Com::{CoTaskMemFree, IBindCtx},
            LibraryLoader::{
                GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS,
                GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, GetModuleFileNameW,
                GetModuleHandleExW,
            },
        },
        UI::Shell::{
            ECF_DEFAULT, ECS_ENABLED, ECS_HIDDEN, IEnumExplorerCommand, IExplorerCommand,
            IExplorerCommand_Impl, IShellItemArray, SHStrDupW, SIGDN_FILESYSPATH,
        },
    },
    core::{BOOL, GUID, PCWSTR, PWSTR, Ref, implement},
};
use windshare_activation::{Activation, EntrySource, MAX_SELECTED_ROOTS, ShareIntent};

#[implement(IExplorerCommand)]
pub struct ShareCommand {
    _lifetime: super::factory::Lifetime,
}
impl ShareCommand {
    pub fn new() -> Self {
        Self {
            _lifetime: super::factory::Lifetime::new(),
        }
    }
}

impl IExplorerCommand_Impl for ShareCommand_Impl {
    fn GetTitle(&self, _: Ref<IShellItemArray>) -> windows::core::Result<PWSTR> {
        // SAFETY: this API has no pointer arguments or initialization requirements.
        let chinese = unsafe { GetUserDefaultUILanguage() } & 0x03ff == 0x04;
        duplicate(if chinese {
            "用 WindShare 分享"
        } else {
            "Share with WindShare"
        })
    }
    fn GetIcon(&self, _: Ref<IShellItemArray>) -> windows::core::Result<PWSTR> {
        let path = module_path()?.with_file_name("windshare.ico");
        duplicate(
            path.to_str()
                .ok_or_else(|| windows::core::Error::from(E_NOTIMPL))?,
        )
    }
    fn GetToolTip(&self, _: Ref<IShellItemArray>) -> windows::core::Result<PWSTR> {
        Err(E_NOTIMPL.into())
    }
    fn GetCanonicalName(&self) -> windows::core::Result<GUID> {
        Ok(GUID::from_u128(crate::command_clsid()))
    }
    fn GetState(&self, items: Ref<IShellItemArray>, _: BOOL) -> windows::core::Result<u32> {
        Ok(if items.is_some() {
            ECS_ENABLED.0 as u32
        } else {
            ECS_HIDDEN.0 as u32
        })
    }
    fn GetFlags(&self) -> windows::core::Result<u32> {
        Ok(ECF_DEFAULT.0 as u32)
    }
    fn EnumSubCommands(&self) -> windows::core::Result<IEnumExplorerCommand> {
        Err(E_NOTIMPL.into())
    }
    fn Invoke(&self, items: Ref<IShellItemArray>, _: Ref<IBindCtx>) -> windows::core::Result<()> {
        let result = invoke(&items);
        if let Err(error) = result {
            windshare_activation::windows::show_failure(&format!(
                "{error:#}\nPlease select local files or folders and try again."
            ));
        }
        // The failure has a visible owner; do not make Explorer add an opaque second dialog.
        Ok(())
    }
}

fn invoke(items: &Ref<IShellItemArray>) -> anyhow::Result<()> {
    let items = items
        .as_ref()
        .ok_or_else(|| anyhow::anyhow!("No selection was supplied"))?;
    // SAFETY: the shell supplied a live IShellItemArray for this invocation.
    let count = unsafe { items.GetCount()? };
    anyhow::ensure!(
        count > 0 && count as usize <= MAX_SELECTED_ROOTS,
        "Select between 1 and {MAX_SELECTED_ROOTS} items"
    );
    let mut roots = Vec::with_capacity(count as usize);
    for index in 0..count {
        // SAFETY: index is bounded by GetCount; returned text is freed after copying native units.
        let path = unsafe { items.GetItemAt(index)?.GetDisplayName(SIGDN_FILESYSPATH)? };
        // SAFETY: SIGDN_FILESYSPATH returns a terminated CoTaskMem string.
        let root = unsafe { PathBuf::from(OsString::from_wide(path.as_wide())) };
        // SAFETY: GetDisplayName transferred this allocation to the caller.
        unsafe {
            CoTaskMemFree(Some(path.0.cast()));
        }
        roots.push(root);
    }
    let intent = ShareIntent::new(EntrySource::Explorer, roots);
    intent.validate().map_err(anyhow::Error::msg)?;
    windshare_activation::windows::dispatch::from_shell(
        &module_path()?.with_file_name("windshare.exe"),
        &Activation::ShareSelection(intent),
    )
}

fn duplicate(text: &str) -> windows::core::Result<PWSTR> {
    let text: Vec<u16> = text.encode_utf16().chain(Some(0)).collect();
    // SAFETY: SHStrDup allocates the terminated string with the allocator required by COM.
    unsafe { SHStrDupW(PCWSTR(text.as_ptr())) }
}

fn module_path() -> windows::core::Result<PathBuf> {
    const MAX_WINDOWS_PATH_UNITS: usize = 32_768;
    let mut module = HMODULE::default();
    // SAFETY: FROM_ADDRESS treats the function pointer as an address within this DLL,
    // not a string; UNCHANGED_REFCOUNT keeps ownership with Explorer.
    unsafe {
        GetModuleHandleExW(
            GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
            PCWSTR((module_path as *const ()).cast::<u16>()),
            &raw mut module,
        )?;
    }
    let mut buffer = vec![0; MAX_WINDOWS_PATH_UNITS];
    // SAFETY: the buffer is writable and the module handle belongs to this DLL.
    let length = unsafe { GetModuleFileNameW(Some(module), &mut buffer) } as usize;
    if length == 0 || length == buffer.len() {
        return Err(windows::core::Error::from_thread());
    }
    Ok(PathBuf::from(OsString::from_wide(&buffer[..length])))
}
