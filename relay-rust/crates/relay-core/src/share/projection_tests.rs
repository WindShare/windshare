use super::*;

#[test]
fn generation_transitions_schedule_recovery_without_coordinator_repairs() {
    const GRACE: Duration = Duration::from_secs(1);
    const RETENTION: Duration = Duration::from_secs(2);
    let fixture = Fixture::new();
    let mut limits = Fixture::limits();
    limits.retirement = RetirementPolicy::Bounded {
        admission_grace: GRACE,
        risk_retention: RETENTION,
        generations: 1,
        residual: Counts::ZERO,
    };
    let registry = |incarnation| {
        Generations::new(
            fixture.identity(incarnation),
            limits,
            AttemptLimiter::new(limits.handshakes.attempts, fixture.at).unwrap(),
        )
    };
    let mut generations = registry(1);
    let peer = connection(1);
    generations.connect(peer, fixture.at).unwrap();
    generations.admit_business(peer).unwrap();
    generations.enable_checkpoints();
    generations.acknowledge_checkpoints();

    generations.detach(peer, fixture.at);
    let deadline = Some((fixture.at.saturating_add(GRACE), Key::Retirement(peer)));
    assert_eq!(generations.next_deadline(), deadline);
    let pending = generations.checkpoint_changes().unwrap();
    assert_eq!(
        pending.connections.iter().copied().collect::<Vec<_>>(),
        [peer]
    );
    assert!(pending.manifest);

    let later = fixture.at.saturating_add(Duration::from_millis(1));
    generations.detach(peer, later);
    assert_eq!(generations.next_deadline(), deadline);
    let record = generations.checkpoint_connection(peer, |_| None).unwrap();
    let mut restored = registry(2);
    restored.enable_checkpoints();
    restored.acknowledge_checkpoints();
    restored.restore_record(&record, false, later).unwrap();
    assert_eq!(restored.next_deadline(), deadline);
    let pending = restored.checkpoint_changes().unwrap();
    assert!(pending.connections.contains(&peer));
    assert!(pending.manifest);
}

#[test]
fn overlapping_owner_changes_flush_once_and_retry_both_journals() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);

    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    activate(&mut relay, key, fixture.at);

    let mut attempted = Vec::new();
    let failed = relay.flush_checkpoints(fixture.at, |target, _| {
        attempted.push(target);
        if target == CheckpointTarget::Manifest {
            Err("manifest unavailable")
        } else {
            Ok(())
        }
    });
    assert_eq!(
        failed,
        Err(CheckpointWriteError::Write {
            target: CheckpointTarget::Manifest,
            error: "manifest unavailable",
        })
    );
    assert_eq!(
        attempted,
        [
            CheckpointTarget::Generation(key.sender()),
            CheckpointTarget::Generation(key.receiver()),
            CheckpointTarget::Manifest,
        ]
    );

    let retried = host.flush(&mut relay, fixture.at);
    assert_eq!(retried.connections, 2);
    assert!(retried.manifest);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    assert_eq!(
        host.flush(&mut relay, fixture.at),
        CheckpointFlush::default()
    );
    let restored = host.restore(&fixture, stored);
    assert_eq!(
        restored.sessions.entries()[&key.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
}

#[test]
fn collection_in_the_same_batch_discards_a_sessions_pending_receiver_record() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);

    assert_eq!(
        relay
            .handle(
                Input::Cancel {
                    participant: key.receiver(),
                    session: key.wire_id(),
                },
                fixture.at
            )
            .refusal,
        None
    );
    for flow in flows {
        assert_eq!(
            relay.handle(Input::Discarded(flow), fixture.at).refusal,
            None
        );
    }
    let _ = relay.handle(Input::Closed(key.receiver()), fixture.at);
    assert!(!relay.generations.contains(&key.receiver()));
    assert!(!relay.sessions.entries().contains_key(&key.wire_id()));

    assert!(host.flush(&mut relay, fixture.at).manifest);
    assert!(!host.written.contains(&key.receiver()));
    assert!(!host.bytes.contains_key(&key.receiver()));
    assert_eq!(
        host.flush(&mut relay, fixture.at),
        CheckpointFlush::default()
    );
}

#[test]
fn control_and_session_deadlines_keep_the_global_tie_order() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let expiry = relay.sessions.entries()[&key.wire_id()]
        .session
        .deadline()
        .unwrap();
    let control = connection(3);
    let handshake =
        crate::handshake::ControlHandshake::open(fixture.identity(1).relay, control, fixture.at);
    let first_frame_budget = handshake.deadline().unwrap().0 - fixture.at.0;
    let opened = UnixMillis(expiry.0 - first_frame_budget);
    assert!(opened >= fixture.at);
    assert_eq!(
        relay.handle(Input::Connected(control), opened).refusal,
        None
    );
    assert_eq!(
        relay.next_deadline(),
        Some((expiry, Key::Handshake(control)))
    );

    let step = relay.handle(Input::Tick, expiry);
    assert_eq!(step.refusal, None);
    assert!(matches!(step.outputs.first(), Some(Output::Close(value)) if *value == control));
    assert_eq!(
        relay.sessions.entries()[&key.wire_id()].session.phase(),
        crate::sessions::Phase::Retired
    );
    assert!(relay.deadline().is_none_or(|deadline| deadline > expiry));
}
