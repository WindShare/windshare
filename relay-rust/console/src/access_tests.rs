use super::*;
use std::collections::BTreeMap;

#[derive(Default)]
struct MemoryStore {
    next_byte: u8,
    owner: [u8; 16],
    throttled: bool,
    enrollments: BTreeMap<[u8; 16], Enrollment>,
    credentials: BTreeMap<[u8; 16], Credential>,
    cancelled: Vec<[u8; 16]>,
}

impl AccessStore for MemoryStore {
    type Error = &'static str;

    fn fill_random(&mut self, dest: &mut [u8]) -> Result<(), Self::Error> {
        self.next_byte = self.next_byte.wrapping_add(1);
        dest.fill(self.next_byte);
        Ok(())
    }

    fn check_enrollment_rate_limit(&mut self, _now: u64) -> Result<(), AccessError<Self::Error>> {
        if self.throttled {
            return Err(ConsoleError::too_many_requests("slow_down", 60_000).into());
        }
        Ok(())
    }

    fn pending_enrollment_count(&self) -> Result<usize, Self::Error> {
        Ok(self
            .enrollments
            .values()
            .filter(|e| !matches!(e.state, EnrollmentState::Installed { .. }))
            .count())
    }

    fn enrollment_by_claimant(&self, claimant: Digest) -> Result<Option<Enrollment>, Self::Error> {
        Ok(self
            .enrollments
            .values()
            .find(|e| e.claim_digest == claimant)
            .cloned())
    }

    fn enrollment(&self, id: &[u8; 16]) -> Result<Option<Enrollment>, Self::Error> {
        Ok(self.enrollments.get(id).cloned())
    }

    fn list_pending_enrollments(&self) -> Result<Vec<Enrollment>, Self::Error> {
        Ok(self
            .enrollments
            .values()
            .filter(|e| e.state == EnrollmentState::Pending)
            .cloned()
            .collect())
    }

    fn save_enrollment(&mut self, enrollment: &Enrollment) -> Result<(), Self::Error> {
        self.enrollments.insert(enrollment.id, enrollment.clone());
        Ok(())
    }

    fn active_credential_count(&self) -> Result<usize, Self::Error> {
        Ok(self
            .credentials
            .values()
            .filter(|c| c.state == CredentialState::Active)
            .count())
    }

    fn credential(&self, id: &[u8; 16]) -> Result<Option<Credential>, Self::Error> {
        Ok(self.credentials.get(id).cloned())
    }

    fn list_active_credentials(&self) -> Result<Vec<Credential>, Self::Error> {
        Ok(self
            .credentials
            .values()
            .filter(|c| c.state == CredentialState::Active)
            .cloned()
            .collect())
    }

    fn list_revoked_credentials(
        &self,
        after: Option<&RevokedCredentialPosition>,
        limit: usize,
    ) -> Result<Vec<Credential>, Self::Error> {
        let mut revoked: Vec<_> = self
            .credentials
            .values()
            .filter_map(|c| match c.state {
                CredentialState::Revoked { at } => Some((at, c.id, c.clone())),
                CredentialState::Active => None,
            })
            .filter(|(at, id, _)| {
                after
                    .is_none_or(|cursor| (*at, *id) < (cursor.revoked_at(), cursor.credential_id()))
            })
            .collect();
        revoked.sort_by_key(|a| std::cmp::Reverse((a.0, a.1)));
        revoked.truncate(limit);
        Ok(revoked.into_iter().map(|(_, _, c)| c).collect())
    }

    fn principal_for_installed(
        &mut self,
        _credential_id: &[u8; 16],
    ) -> Result<[u8; 16], Self::Error> {
        Ok(self.owner)
    }

    fn save_credential(&mut self, credential: &Credential) -> Result<bool, Self::Error> {
        let previous = self.credentials.insert(credential.id, credential.clone());
        Ok(previous.as_ref() != Some(credential))
    }

    fn cancel_installed_credential(&mut self, id: &[u8; 16], now: u64) -> Result<(), Self::Error> {
        self.cancelled.push(*id);
        if let Some(credential) = self.credentials.get_mut(id) {
            credential.revoke(now);
        }
        Ok(())
    }
}

fn rejected<T>(result: Result<T, AccessError<&'static str>>) -> ConsoleError {
    match result.map(drop) {
        Err(AccessError::Rejected(error)) => error,
        Err(AccessError::Store(error)) => panic!("unexpected store error: {error}"),
        Ok(()) => panic!("expected rejection"),
    }
}

#[test]
fn enrollment_activates_only_on_install_and_supports_recovery_and_cancel() {
    let mut store = MemoryStore {
        owner: [9; 16],
        ..MemoryStore::default()
    };
    let secret = [7_u8; 32];
    let digest = text_id(&Digest::of(&secret).0);
    let request = json!({"device_name": "Laptop", "claim_digest": digest});

    let first = enroll(&mut store, &request, 100).unwrap();
    assert!(first.created);
    let retry = enroll(&mut store, &request, 101).unwrap();
    assert!(!retry.created);
    assert_eq!(first.enrollment.id, retry.enrollment.id);

    let pending = list_pending_enrollments(&store).unwrap();
    assert_eq!(pending["enrollments"][0]["created_at"], 100);

    let claim_body = json!({
        "enrollment_id": text_id(&first.enrollment.id),
        "claim_secret": text_id(&secret),
    });
    assert_eq!(
        rejected(claim(&mut store, &claim_body, 102)),
        ConsoleError::from_refusal(Refusal::Pending)
    );
    assert_eq!(
        rejected(approve(
            &mut store,
            &json!({"enrollment_id": text_id(&first.enrollment.id), "user_code": "WRONG"}),
            103,
        )),
        ConsoleError::from_refusal(Refusal::AccessDenied)
    );
    approve(
        &mut store,
        &json!({
            "enrollment_id": text_id(&first.enrollment.id),
            "user_code": first.enrollment.user_code,
        }),
        103,
    )
    .unwrap();
    assert_eq!(
        list_pending_enrollments(&store).unwrap(),
        json!({"enrollments": []})
    );

    let claimed = claim(&mut store, &claim_body, 102 + 5_000).unwrap();
    assert_eq!(claimed.body["expires_at"], first.enrollment.expires_at);
    assert_eq!(store.active_credential_count().unwrap(), 0);

    let install_first = installed(&mut store, &claim_body, 5_200).unwrap();
    assert!(install_first.newly_installed);
    assert_eq!(store.active_credential_count().unwrap(), 1);
    let install_retry = installed(&mut store, &claim_body, 5_300).unwrap();
    assert!(!install_retry.newly_installed);

    let cancelled = cancel(&mut store, &claim_body, 5_400).unwrap();
    assert!(cancelled.found);
    assert_eq!(cancelled.cancelled_credential, Some(claimed.credential_id));
    assert_eq!(store.cancelled, vec![claimed.credential_id]);
    assert_eq!(
        rejected(installed(&mut store, &claim_body, 5_500)),
        ConsoleError::from_refusal(Refusal::Cancelled)
    );
}

#[test]
fn service_keys_validate_names_and_paginate_revoked_history() {
    let mut store = MemoryStore::default();
    assert_eq!(
        rejected(create_service_key(
            &mut store,
            &json!({"name": "   "}),
            "https://relay.example",
            10,
        )),
        ConsoleError::bad_request("invalid_name")
    );
    let created = create_service_key(
        &mut store,
        &json!({"name": "CI"}),
        "https://relay.example",
        10,
    )
    .unwrap();
    assert_eq!(
        created.body["configuration"]["url"],
        "https://relay.example"
    );
    assert_eq!(
        list_active_credentials(&store).unwrap()["credentials"]
            .as_array()
            .unwrap()
            .len(),
        1
    );

    let revoked = revoke_credential(
        &mut store,
        &json!({"credential_id": text_id(&created.credential.id), "terminate_shares": true}),
        20,
    )
    .unwrap();
    assert!(revoked.changed);
    assert!(revoked.terminate_shares);

    let history = list_revoked_credentials(&store, None).unwrap();
    assert!(!history.continued);
    assert!(!history.has_more);
    assert_eq!(history.body["credentials"].as_array().unwrap().len(), 1);
    assert_eq!(
        rejected(list_revoked_credentials(&store, Some("bad"))),
        ConsoleError::bad_request("invalid_cursor")
    );
    assert_eq!(
        rejected(revoke_credential(
            &mut store,
            &json!({"credential_id": text_id(&[99; 16])}),
            21,
        )),
        ConsoleError::not_found()
    );
}
