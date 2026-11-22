//! Service identity and enrollment policy. Hosts own storage, entropy, clocks and admin authentication.
#![no_std]
extern crate alloc;

mod attribution;
mod cbor;
mod credential;
pub mod device_login;
pub mod entitlement;
pub mod governor;
pub mod probe;
pub mod restrictions;
pub mod ticket;
pub mod trust;
pub mod usage;
pub use attribution::{AccountId, Attribution, CredentialId, Issuer, Principal};
mod enrollment;

pub use credential::{Credential, CredentialState, Digest, Refusal};
pub use enrollment::{
    ENROLLMENT_LIFETIME_MS, Enrollment, EnrollmentState, POLL_INTERVAL_MS, Poll, validate_name,
};

pub const PRIVATE_PRINCIPAL: &str = "owner";
pub const ANONYMOUS_PRINCIPAL: &str = "anonymous";
