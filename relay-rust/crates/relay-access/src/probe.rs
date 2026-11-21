//! Probe grants use the normal ticket wire format. Their signed credential
//! commits to a single relay identity and a single synthetic share.
use crate::{AccountId, CredentialId, ticket::Ticket};
use sha2::{Digest, Sha256};

pub const PROBE_ACCOUNT: AccountId = *b"windshare-probe!";
pub const LIFETIME: u64 = 180;

pub fn credential(node: &[u8; 32], share: &[u8; 12]) -> CredentialId {
    let mut hash = Sha256::new();
    hash.update(b"windshare/probe-scope/v1\0");
    hash.update(node);
    hash.update(share);
    let mut id = [0; 16];
    id.copy_from_slice(&hash.finalize()[..16]);
    id
}

pub fn permits(ticket: Ticket, node: Option<&[u8; 32]>, share: &[u8; 12]) -> bool {
    ticket.account != PROBE_ACCOUNT
        || (ticket.expires_at.saturating_sub(ticket.issued_at) <= LIFETIME
            && node.is_some_and(|key| credential(key, share) == ticket.credential))
}
