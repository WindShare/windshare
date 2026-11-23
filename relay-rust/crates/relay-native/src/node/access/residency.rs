use super::{Attribution, Refusal, RefusalReason, Service, ShareId};
use crate::connection::AdmissionPriority;
use windshare_relay_core::access::RegistrationDecision;

/// A resident share owns its slot across receiver joins and sender reconnects.
pub struct Residency {
    access: Service,
    share: ShareId,
    attribution: Attribution,
}

impl Residency {
    pub const fn attribution(&self) -> Attribution {
        self.attribution
    }
}

impl Drop for Residency {
    fn drop(&mut self) {
        let mut state = self.access.write();
        if let Some((_, references)) = state.residents.get_mut(&self.share) {
            *references -= 1;
            if *references == 0 {
                state.residents.remove(&self.share);
            }
        }
    }
}

impl Service {
    pub(crate) fn restore(
        &self,
        share: ShareId,
        attribution: Attribution,
    ) -> Result<Residency, RegistrationDecision> {
        let mut state = self.write();
        let entry = state.residents.entry(share).or_insert((attribution, 0));
        if entry.0 != attribution {
            return Err(RegistrationDecision::Denied);
        }
        entry.1 += 1;
        drop(state);
        Ok(Residency {
            access: self.clone(),
            share,
            attribution,
        })
    }

    pub(crate) fn reserve(
        &self,
        share: ShareId,
        attribution: Attribution,
    ) -> Result<Residency, Refusal> {
        let mut state = self.write();
        if state.admission() == super::Admission::Draining {
            return Err(Refusal::unavailable(RefusalReason::NodeDraining));
        }
        if let Some(reason) = state.denial_reason(share, attribution) {
            return Err(Refusal::denied(reason));
        }
        if let Some((existing, references)) = state.residents.get_mut(&share) {
            if *existing != attribution {
                return Err(Refusal::denied(RefusalReason::AttributionMismatch));
            }
            *references += 1;
        } else {
            let priority = state.priority_for(attribution);
            let principal_limited = priority == AdmissionPriority::Authenticated && {
                let principal = attribution.principal(*share.as_bytes());
                state
                    .residents
                    .values()
                    .filter(|(value, _)| value.principal(*share.as_bytes()) == principal)
                    .count()
                    >= state.policy.shares_per_principal
            };
            if principal_limited {
                return Err(Refusal::unavailable(RefusalReason::PrincipalShareLimit));
            }
            if state.residents.len() >= state.share_limit(priority) {
                return Err(Refusal::unavailable(RefusalReason::ResidentShareCapacity));
            }
            state.residents.insert(share, (attribution, 1));
        }
        drop(state);
        Ok(Residency {
            access: self.clone(),
            share,
            attribution,
        })
    }
}
