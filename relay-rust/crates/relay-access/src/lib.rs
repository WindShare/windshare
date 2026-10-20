//! Service identity and enrollment policy. Hosts own storage, entropy, clocks and admin authentication.
#![no_std]
extern crate alloc;

mod credential;
mod enrollment;

pub use credential::{Attribution, Credential, CredentialState, Digest, Refusal};
pub use enrollment::{ENROLLMENT_LIFETIME_MS, Enrollment, EnrollmentState, POLL_INTERVAL_MS, Poll};

pub const PRIVATE_PRINCIPAL: &str = "owner";
pub const ANONYMOUS_PRINCIPAL: &str = "anonymous";
