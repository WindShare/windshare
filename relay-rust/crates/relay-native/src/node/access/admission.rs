use super::{Service, State, devices::CredentialEntry};
use crate::connection::AdmissionPriority;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::sync::Arc;
use windshare_relay_access::{
    Attribution, Digest,
    ticket::{MAX_TICKET_BYTES, Ticket, VerifiedTicket},
    trust::Error,
};
use windshare_relay_core::{access::RegistrationDecision, identifiers::ShareId};

const ENCODED_SIGNATURE_BYTES: usize = (ed25519_dalek::SIGNATURE_LENGTH * 4).div_ceil(3);
const MAX_ENCODED_TICKET_BYTES: usize = (MAX_TICKET_BYTES * 4).div_ceil(3);

#[derive(Clone)]
pub struct Bearer {
    digest: Digest,
    ticket: TicketEvidence,
}

#[derive(Clone)]
enum TicketEvidence {
    Absent,
    Unverified(Arc<str>),
    Verified(Arc<VerifiedTicket>),
}

impl Bearer {
    pub(crate) fn new(token: &str) -> Self {
        let ticket =
            if (ENCODED_SIGNATURE_BYTES + 1..=MAX_ENCODED_TICKET_BYTES).contains(&token.len()) {
                TicketEvidence::Unverified(token.into())
            } else {
                TicketEvidence::Absent
            };
        Self {
            digest: Digest::of(token.as_bytes()),
            ticket,
        }
    }

    fn ticket_proof(&self, access: &Service, now: u64) -> Option<Arc<VerifiedTicket>> {
        match &self.ticket {
            TicketEvidence::Absent => None,
            TicketEvidence::Verified(ticket) => Some(Arc::clone(ticket)),
            TicketEvidence::Unverified(encoded) => {
                let signed = URL_SAFE_NO_PAD.decode(encoded.as_bytes()).ok()?;
                VerifiedTicket::verify_with(&signed, now, |issuer, number| {
                    let state = access.state.lock().expect("node access state");
                    if !state.active_issuers.contains(&issuer) {
                        return Err(Error::Untrusted);
                    }
                    state.authorities.trusted.current(issuer, number, now)
                })
                .ok()
                .map(Arc::new)
            }
        }
    }
}

enum AuthorizedIdentity<'a> {
    Anonymous,
    Local {
        attribution: Attribution,
        entry: &'a CredentialEntry,
    },
    Ticket(Ticket),
}

impl AuthorizedIdentity<'_> {
    const fn attribution(&self) -> Attribution {
        match self {
            Self::Anonymous => Attribution::Anonymous,
            Self::Local { attribution, .. } => *attribution,
            Self::Ticket(ticket) => ticket.attribution(),
        }
    }
}

impl Service {
    pub(crate) fn admission_priority(
        &self,
        share: ShareId,
        bearer: Option<&mut Bearer>,
        now: u64,
    ) -> AdmissionPriority {
        let state = self.state.lock().expect("node access state");
        if let Some(priority) = state.known_priority(share, bearer.as_deref()) {
            return priority;
        }
        drop(state);
        let Some(bearer) = bearer else {
            return AdmissionPriority::Anonymous;
        };
        let Some(proof) = bearer.ticket_proof(self, now) else {
            return AdmissionPriority::Anonymous;
        };

        let state = self.state.lock().expect("node access state");
        let priority = state
            .known_priority(share, Some(bearer))
            .unwrap_or_else(|| {
                if state
                    .authorized_identity(share, Some(bearer), Some(&proof), now)
                    .is_some()
                {
                    AdmissionPriority::Authenticated
                } else {
                    AdmissionPriority::Anonymous
                }
            });
        drop(state);
        // Keep proof of the signature on this connection. Policy is checked again at REGISTER.
        bearer.ticket = TicketEvidence::Verified(proof);
        priority
    }

    pub(crate) fn authorize(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        now: u64,
    ) -> RegistrationDecision {
        // A previously untrusted signing generation may have become available since upgrade.
        let proof = bearer.and_then(|bearer| bearer.ticket_proof(self, now));
        let mut state = self.state.lock().expect("node access state");
        let Some(identity) = state.authorized_identity(share, bearer, proof.as_deref(), now) else {
            return RegistrationDecision::Denied;
        };
        let attribution = identity.attribution();
        match identity {
            AuthorizedIdentity::Anonymous => {}
            AuthorizedIdentity::Local { entry, .. } => entry.record_use(now.saturating_mul(1000)),
            AuthorizedIdentity::Ticket(ticket) => {
                state.quotas.advance(
                    (ticket.issuer, ticket.account),
                    super::Quota::from_snapshot(ticket.entitlement, now),
                    now,
                );
            }
        }
        drop(state);
        RegistrationDecision::Granted(attribution)
    }
}

impl State {
    fn known_priority(&self, share: ShareId, bearer: Option<&Bearer>) -> Option<AdmissionPriority> {
        let resident = self
            .residents
            .get(&share)
            .map(|(attribution, _)| *attribution)
            .filter(|attribution| {
                *attribution != Attribution::Anonymous && !self.denied(share, *attribution)
            });
        let local = bearer.and_then(|bearer| self.credentials.get(bearer.digest));
        let local_attribution = local.and_then(|entry| self.local_attribution(entry));
        if resident.is_some_and(|attribution| self.owner(attribution))
            || local_attribution.is_some_and(|attribution| self.owner(attribution))
        {
            Some(AdmissionPriority::Owner)
        } else if resident.is_some()
            || local_attribution.is_some_and(|attribution| !self.denied(share, attribution))
        {
            Some(AdmissionPriority::Authenticated)
        } else {
            local.map(|_| AdmissionPriority::Anonymous)
        }
    }

    fn authorized_identity(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        proof: Option<&VerifiedTicket>,
        now: u64,
    ) -> Option<AuthorizedIdentity<'_>> {
        let identity = match bearer {
            None if self.anonymous() => AuthorizedIdentity::Anonymous,
            None => return None,
            Some(bearer) => match self.credentials.get(bearer.digest) {
                Some(entry) => AuthorizedIdentity::Local {
                    attribution: self.local_attribution(entry)?,
                    entry,
                },
                None => {
                    AuthorizedIdentity::Ticket(proof?.current(&self.authorities.trusted, now).ok()?)
                }
            },
        };
        (!self.denied(share, identity.attribution())).then_some(identity)
    }

    fn local_attribution(&self, entry: &CredentialEntry) -> Option<Attribution> {
        let key = &entry.value;
        let attribution = key.active_attribution()?;
        Some(
            self.credentials
                .account(key.id)
                .map_or(attribution, |binding| Attribution::Account {
                    issuer: binding.issuer,
                    account: binding.account,
                    credential: key.id,
                }),
        )
    }
}
