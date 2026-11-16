use super::handles::{Handle, LocalAllocation, take_local_string, wide};
use windows::{
    Win32::{
        Foundation::HANDLE,
        Security::{
            Authorization::{
                ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
                SDDL_REVISION_1,
            },
            GetTokenInformation, PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES, TOKEN_GROUPS,
            TOKEN_QUERY, TokenLogonSid,
        },
        System::Threading::{GetCurrentProcess, OpenProcessToken},
    },
    core::{PCWSTR, PWSTR},
};

pub fn logon_sid() -> windows::core::Result<String> {
    let mut token = HANDLE::default();
    // SAFETY: output points to a local handle; the process pseudo-handle is borrowed.
    unsafe {
        OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &raw mut token)?;
    }
    let token = Handle::new(token)?;
    let mut length = 0;
    // SAFETY: the null-buffer query only writes the required buffer length.
    let _ = unsafe { GetTokenInformation(token.raw(), TokenLogonSid, None, 0, &raw mut length) };
    let mut buffer = vec![0_usize; (length as usize).div_ceil(size_of::<usize>())];
    // SAFETY: the sized, aligned buffer remains live while TOKEN_GROUPS and SID are read.
    unsafe {
        GetTokenInformation(
            token.raw(),
            TokenLogonSid,
            Some(buffer.as_mut_ptr().cast()),
            length,
            &raw mut length,
        )?;
        let groups = &*buffer.as_ptr().cast::<TOKEN_GROUPS>();
        if groups.GroupCount != 1 {
            return Err(windows::core::Error::from_thread());
        }
        let mut text = PWSTR::null();
        ConvertSidToStringSidW(groups.Groups[0].Sid, &raw mut text)?;
        take_local_string(text)
    }
}

pub struct Security(LocalAllocation);
impl Security {
    pub fn new(sid: &str) -> windows::core::Result<Self> {
        // A logon SID identifies both the user and this interactive logon. User-SID
        // ACLs alone would allow another session of the same user to inject work.
        let sddl = wide(format!("D:P(A;;GA;;;{sid})"));
        let mut descriptor = PSECURITY_DESCRIPTOR::default();
        // SAFETY: Windows allocates a self-relative descriptor freed by LocalFree.
        unsafe {
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                PCWSTR(sddl.as_ptr()),
                SDDL_REVISION_1,
                &raw mut descriptor,
                None,
            )?;
        }
        Ok(Self(LocalAllocation(descriptor.0)))
    }

    pub fn attributes(&self) -> SECURITY_ATTRIBUTES {
        SECURITY_ATTRIBUTES {
            nLength: u32::try_from(size_of::<SECURITY_ATTRIBUTES>())
                .expect("security attributes fit u32"),
            lpSecurityDescriptor: self.0.0,
            bInheritHandle: false.into(),
        }
    }
}
