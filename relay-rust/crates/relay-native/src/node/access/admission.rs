use super::{Service, State, devices::CredentialEntry};
use crate::connection::AdmissionPriority;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::sync::Arc;
use windshare_relay_access::{
    Attribution, Digest,
    ticket::{MAX_TICKET_BYTES, Ticket, VerificationError, VerifiedTicket},
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

    fn ticket_proof(
        &self,
        access: &Service,
        now: u64,
    ) -> Result<Arc<VerifiedTicket>, VerificationError> {
        match &self.ticket {
            TicketEvidence::Absent => Err(Error::Encoding.into()),
            TicketEvidence::Verified(ticket) => Ok(Arc::clone(ticket)),
            TicketEvidence::Unverified(encoded) => {
                let signed = URL_SAFE_NO_PAD
                    .decode(encoded.as_bytes())
                    .map_err(|_| Error::Encoding)?;
                VerifiedTicket::verify_with(&signed, now, |issuer| {
                    let state = access.state.lock().expect("node access state");
                    if state.active_issuers.contains(&issuer) {
                        state.authorities.trusted.generation(issuer)
                    } else {
                        None
                    }
                })
                .map(Arc::new)
            }
        }
    }
}

enum AdmissionRefusal {
    Denied,
    Ticket(VerificationError),
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
        let Ok(proof) = bearer.ticket_proof(self, now) else {
            return AdmissionPriority::Anonymous;
        };

        let state = self.state.lock().expect("node access state");
        let priority = state
            .known_priority(share, Some(bearer))
            .unwrap_or_else(|| {
                if state
                    .authorized_identity(share, Some(bearer), Some(Ok(proof.as_ref())), now)
                    .is_ok()
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

    pub(crate) fn egress_owner(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        forwarded: Option<Attribution>,
    ) -> Option<bool> {
        let state = self.state.lock().expect("node access state");
        // Receivers normally present no owner key. Once a share is registered,
        // its sender's attribution governs both directions and their controls.
        // An explicit bearer that does not match an active local credential
        // (such as an official account ticket) cannot represent an owner share.
        let attribution = forwarded
            .or_else(|| {
                state
                    .residents
                    .get(&share)
                    .map(|(attribution, _)| *attribution)
            })
            .or_else(|| {
                let entry = state.credentials.get(bearer?.digest)?;
                state
                    .local_attribution(entry)
                    .filter(|value| !state.denied(share, *value))
            });
        let owner = attribution
            .map(|value| state.owner(value))
            .or_else(|| bearer.map(|_| false));
        drop(state);
        owner
    }

    pub(crate) fn authorize(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        now: u64,
    ) -> RegistrationDecision {
        // A previously untrusted signing generation may have become available since upgrade.
        let proof = bearer.map(|bearer| bearer.ticket_proof(self, now));
        let proof = proof
            .as_ref()
            .map(|result| result.as_deref().map_err(|error| *error));
        let mut state = self.state.lock().expect("node access state");
        let identity = match state.authorized_identity(share, bearer, proof, now) {
            Ok(identity) => identity,
            Err(AdmissionRefusal::Denied) => return RegistrationDecision::Denied,
            Err(AdmissionRefusal::Ticket(error)) => return state.ticket_refusal(share, error),
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
        proof: Option<Result<&VerifiedTicket, VerificationError>>,
        now: u64,
    ) -> Result<AuthorizedIdentity<'_>, AdmissionRefusal> {
        let identity = match bearer {
            None if self.anonymous() => AuthorizedIdentity::Anonymous,
            None => return Err(AdmissionRefusal::Denied),
            Some(bearer) => {
                if let Some(entry) = self.credentials.get(bearer.digest) {
                    AuthorizedIdentity::Local {
                        attribution: self
                            .local_attribution(entry)
                            .ok_or(AdmissionRefusal::Denied)?,
                        entry,
                    }
                } else {
                    let ticket = proof
                        .ok_or(AdmissionRefusal::Denied)?
                        .and_then(|proof| proof.current(&self.authorities.trusted, now))
                        .map_err(AdmissionRefusal::Ticket)?;
                    AuthorizedIdentity::Ticket(ticket)
                }
            }
        };
        (!self.denied(share, identity.attribution()))
            .then_some(identity)
            .ok_or(AdmissionRefusal::Denied)
    }

    fn ticket_refusal(&self, share: ShareId, error: VerificationError) -> RegistrationDecision {
        let VerificationError::GenerationMismatch {
            issuer,
            presented_generation,
            installed_generation,
        } = error
        else {
            return RegistrationDecision::Denied;
        };
        if !self.active_issuers.contains(&issuer)
            || self.disabled_controller() == Some(issuer)
            || self
                .restrictions
                .values()
                .any(|restrictions| restrictions.shares.contains(share.as_bytes()))
        {
            return RegistrationDecision::Denied;
        }

        let direction = if presented_generation > installed_generation {
            "ticket_ahead"
        } else {
            "ticket_behind"
        };
        tracing::debug!(
            event = "ticket_generation_mismatch",
            ?share,
            ?issuer,
            presented_generation,
            installed_generation,
            direction,
        );
        RegistrationDecision::SERVICE_UNAVAILABLE
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
