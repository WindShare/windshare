use super::*;

fn wake(
    fixture: &Fixture,
    relay: &Relay,
    stored: Option<Record>,
    surviving: &[ConnectionRef],
    at: UnixMillis,
) -> (Relay, Turn) {
    let bytes = relay.checkpoint().unwrap().encode();
    let checkpoint = Checkpoint::decode(&bytes, Fixture::limits()).unwrap();
    Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            record: stored,
            surviving,
            checkpoint: Some(&checkpoint),
        },
        Trace::default(),
        at,
    )
    .unwrap()
}

#[test]
fn unissued_challenge_restarts_random_request_without_extending_handshake() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    let request =
        challenge(&fixture.control(&mut relay, connection(1), "registerInitB64", fixture.at));
    let deadline = relay.deadline();
    let (mut restored, turn) = wake(
        &fixture,
        &relay,
        None,
        &[connection(1)],
        UnixMillis(fixture.at.0 + 1),
    );
    assert_eq!(restored.deadline(), deadline);
    let seed = || {
        challenge_seed(
            fixture.text("challengeIdB64"),
            fixture.text("challengeNonceB64"),
        )
    };
    assert!(
        restored
            .handle(
                Input::Challenge {
                    request,
                    seed: seed()
                },
                fixture.at
            )
            .refusal
            .is_some()
    );
    let current = challenge(&turn);
    assert_ne!(current, request);
    assert_eq!(
        restored
            .handle(
                Input::Challenge {
                    request: current,
                    seed: seed()
                },
                fixture.at
            )
            .refusal,
        None
    );
    assert_eq!(
        fixture
            .control(&mut restored, connection(1), "registerProofB64", fixture.at)
            .refusal,
        None
    );
    let upload = fixture.control(
        &mut restored,
        connection(1),
        "descriptorUploadB64",
        fixture.at,
    );
    assert!(matches!(work(&upload).operation, Storage::Publish { .. }));
}

#[test]
fn awaiting_descriptor_reauthenticates_proof_after_hibernation() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    let requested = fixture.control(&mut relay, connection(1), "registerInitB64", fixture.at);
    let seed = challenge_seed(
        fixture.text("challengeIdB64"),
        fixture.text("challengeNonceB64"),
    );
    let _ = relay.handle(
        Input::Challenge {
            request: challenge(&requested),
            seed,
        },
        fixture.at,
    );
    let _ = fixture.control(&mut relay, connection(1), "registerProofB64", fixture.at);
    let (mut restored, turn) = wake(&fixture, &relay, None, &[connection(1)], fixture.at);
    assert!(turn.outputs.is_empty());
    let publish = work(&fixture.control(
        &mut restored,
        connection(1),
        "descriptorUploadB64",
        fixture.at,
    ));
    let durable = work(&complete(&mut restored, &publish, fixture.at));
    assert!(
        complete(&mut restored, &durable, fixture.at)
            .outputs
            .iter()
            .any(|output| matches!(
                output,
                Output::Send {
                    control: Control::Registered(_),
                    ..
                }
            ))
    );
}

fn begin_resume(fixture: &Fixture, relay: &mut Relay, slot: u64) -> UnixMillis {
    let at = UnixMillis(
        unix_seconds(fixture.text("resumeExpiresAt")).start().0 - REGISTRATION_BUDGET - 1,
    );
    let _ = relay.handle(Input::Connected(connection(slot)), at);
    let _ = fixture.control(relay, connection(slot), "resumeInitB64", at);
    let requested = fixture.control(relay, connection(slot), "resumeCredentialB64", at);
    let seed = challenge_seed(
        fixture.text("resumeChallengeIdB64"),
        fixture.text("resumeChallengeNonceB64"),
    );
    assert_eq!(
        relay
            .handle(
                Input::Challenge {
                    request: challenge(&requested),
                    seed
                },
                at
            )
            .refusal,
        None
    );
    at
}

#[test]
fn resume_declares_a_new_connection_capacity_without_reclaiming_the_old_owner() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    let old_capacity = relay.connections[&connection(1)].ledger.receive_capacity();
    let at = begin_resume(&fixture, &mut relay, 3);
    let resumption = work(&fixture.control(&mut relay, connection(3), "resumeProofB64", at));
    let _ = complete(&mut relay, &resumption, at);
    let new_capacity = RegisterInit::decode(&fixture.frame("resumeInitB64"))
        .unwrap()
        .receive_capacity();
    assert_eq!(
        relay.connections[&connection(3)].ledger.receive_capacity(),
        Some(new_capacity)
    );
    assert_ne!(old_capacity, Some(new_capacity));
    assert_eq!(
        relay.connections[&connection(1)].ledger.receive_capacity(),
        old_capacity
    );
    assert_eq!(
        relay.connections[&connection(1)].ledger.usage().submitted,
        Counts::new(1, 21)
    );
}

#[test]
fn pending_resume_never_rebinds_to_a_replacement_owner_on_wake() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    begin_resume(&fixture, &mut relay, 3);
    let at = begin_resume(&fixture, &mut relay, 4);
    let resume = work(&fixture.control(&mut relay, connection(4), "resumeProofB64", at));
    let stored = next_record(&resume);
    let _ = complete(&mut relay, &resume, at);
    let _ = relay.handle(Input::Closed(connection(1)), at);
    let (mut restored, turn) = wake(
        &fixture,
        &relay,
        Some(stored),
        &[connection(3), connection(4)],
        at,
    );
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == connection(3)))
    );
    let refused = fixture.control(&mut restored, connection(3), "resumeProofB64", at);
    assert!(!refused.outputs.iter().any(|output| matches!(
        output,
        Output::Work(_)
            | Output::Send {
                control: Control::Registered(_),
                ..
            }
    )));
}
