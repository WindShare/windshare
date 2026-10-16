use core::ops::Range;

use super::*;
use crate::sessions::{MAX_SNAPSHOT_BYTES, Retirement, SessionSnapshot, SnapshotFault};

// Byte ranges of an encoded snapshot after its format and phase bytes: relay,
// share, instance, wire ID, owner epoch and connection, admitting incarnation,
// receiver connection, admission sequence, then any provisional deadline.
const RELAY: Range<usize> = 2..34;
const INSTANCE: Range<usize> = 46..62;
const WIRE_ID: Range<usize> = 62..70;
const EPOCH: Range<usize> = 70..78;
const SENDER: Range<usize> = 78..94;
const ADMITTED_BY: Range<usize> = 94..110;
const RECEIVER: Range<usize> = 110..126;
const SEQUENCE: Range<usize> = 126..134;
const DEADLINE: Range<usize> = 134..142;
const DEADLINE_BYTES: usize = 8;

fn woken() -> Incarnation {
    Incarnation::new([2; 16]).unwrap()
}

/// Rebuilds the actor as a hibernating host does: its record, surviving
/// sockets and the snapshots their attachments hold all pass through bytes.
fn wake(
    fixture: &Fixture,
    stored: Record,
    connections: &[ConnectionRef],
    snapshots: &[SessionSnapshot],
    at: UnixMillis,
) -> (
    ShareLifecycle<Recorder>,
    Action<'static>,
    Vec<SessionRestore>,
) {
    let record = Record::decode(stored.encode().as_bytes()).unwrap();
    let sessions: Vec<_> = snapshots
        .iter()
        .map(|snapshot| SessionSnapshot::decode(snapshot.encode().as_bytes()).unwrap())
        .collect();
    let mut outcomes = Vec::new();
    let restoration = Restoration {
        record: Some(record),
        connections,
        sessions: &sessions,
    };
    let (engine, action) = ShareLifecycle::restore(
        fixture.relay(),
        fixture.share(),
        restoration,
        woken(),
        Recorder::default(),
        at,
        |index, outcome| {
            assert_eq!(index, outcomes.len());
            outcomes.push(outcome);
        },
    )
    .unwrap();
    (engine, action, outcomes)
}

fn restored(outcome: SessionRestore) -> Session {
    match outcome {
        SessionRestore::Restored(session) => session,
        SessionRestore::Retired(retirement) => panic!("expected a live session: {retirement:?}"),
    }
}

fn retired(outcome: &SessionRestore) -> Retirement {
    match outcome {
        SessionRestore::Retired(retirement) => *retirement,
        SessionRestore::Restored(session) => panic!("expected retirement: {session:?}"),
    }
}

fn edited(snapshot: SessionSnapshot, range: Range<usize>, value: &[u8]) -> SessionSnapshot {
    let mut bytes = snapshot.encode().as_bytes().to_vec();
    bytes[range].copy_from_slice(value);
    SessionSnapshot::decode(&bytes).unwrap()
}

#[test]
fn hibernation_keeps_surviving_sessions_and_their_queued_permits() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let sender = published.owner.connection();
    let mut active = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    activate(&engine, &mut active, fixture.at);
    let mut provisional = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let up = forward(&engine, &mut active, connection(2, 1), fixture.at);
    let down = forward(&engine, &mut provisional, sender, fixture.at);
    let snapshots = [active.snapshot().unwrap(), provisional.snapshot().unwrap()];

    let survivors = [sender, connection(2, 1), connection(3, 1)];
    let (mut woken_engine, action, outcomes) = wake(
        &fixture,
        published.record(),
        &survivors,
        &snapshots,
        fixture.at,
    );
    assert_eq!(action, Action::None);
    let [first, second] = <[_; 2]>::try_from(outcomes).unwrap();
    let (mut active, provisional) = (restored(first), restored(second));
    assert_eq!(active.key(), snapshots[0].key());
    assert_eq!(active.phase(), SessionPhase::Active);
    assert_eq!(provisional.key(), snapshots[1].key());
    assert_eq!(provisional.deadline(), snapshots[1].deadline());
    // Permits queued before hibernation remain valid against the new actor.
    assert!(active.check_forward(&woken_engine, up, fixture.at).is_ok());
    assert!(
        provisional
            .check_forward(&woken_engine, down, fixture.at)
            .is_ok()
    );
    assert_eq!(
        woken_engine.handle(Event::Tick, fixture.at).revocation,
        None
    );
    let observations = &woken_engine.observer.0;
    for (index, snapshot) in snapshots.iter().enumerate() {
        let observation = observations[index + 1];
        assert_eq!(observation.event, ObservedEvent::RestoreSession(index));
        assert_eq!(
            observation.decision,
            Decision::SessionRestored(snapshot.key())
        );
        assert_eq!(observation.operation, Some(snapshot.key().admission()));
        assert_eq!(observation.ownership, Some(published.owner));
    }

    // New admissions use the fresh incarnation, so even a reused sequence
    // never names a surviving admission.
    join(&fixture, &mut woken_engine, connection(4, 1), 3, fixture.at);
    let fresh = join(&fixture, &mut woken_engine, connection(5, 1), 4, fixture.at);
    let survivor = active.key().admission();
    assert_eq!(fresh.key().admission().sequence(), survivor.sequence());
    assert_ne!(fresh.key().admission(), survivor);

    // Restored sessions stay bound to the ownership they joined.
    let revocation = fixture
        .resume(&mut woken_engine, connection(1, 2))
        .revocation
        .unwrap();
    assert_eq!(revocation.owner(), published.owner);
    let cleanup = active.handle(
        &woken_engine,
        SessionEvent::Revoke(revocation),
        fixture.resume_at,
        &mut (),
    );
    assert_eq!(
        cleanup.retirement.unwrap().reason,
        RetirementReason::OwnerReplaced
    );
}

#[test]
fn a_receiver_lost_during_hibernation_retires_only_its_session() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let sender = published.owner.connection();
    let mut kept = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    activate(&engine, &mut kept, fixture.at);
    let permit = forward(&engine, &mut kept, sender, fixture.at);
    let mut lost = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    activate(&engine, &mut lost, fixture.at);
    let snapshots = [kept.snapshot().unwrap(), lost.snapshot().unwrap()];

    let (woken_engine, action, outcomes) = wake(
        &fixture,
        published.record(),
        &[sender, connection(2, 1)],
        &snapshots,
        fixture.at,
    );
    assert_eq!(action, Action::None);
    let retirement = retired(&outcomes[1]);
    assert_eq!(retirement.session, lost.key());
    assert_eq!(
        retirement.reason,
        RetirementReason::Ended(EndReason::ReceiverDisconnected)
    );
    assert_eq!(
        woken_engine.observer.0.last().unwrap().decision,
        Decision::SessionRetired(retirement)
    );
    let [first, _] = <[_; 2]>::try_from(outcomes).unwrap();
    assert!(
        restored(first)
            .check_forward(&woken_engine, permit, fixture.at)
            .is_ok()
    );
}

#[test]
fn a_lost_or_stopped_owner_retires_every_surviving_session() {
    let fixture = Fixture::new();
    for stopped in [false, true] {
        let mut engine = fixture.engine();
        let published = fixture.register(&mut engine);
        let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
        activate(&engine, &mut session, fixture.at);
        let snapshot = session.snapshot().unwrap();
        let (stored, survivors, reason) = if stopped {
            let stop = Event::Stop {
                connection: connection(4, 1),
                authority: stopping::authority(&fixture, connection(4, 1)),
            };
            let Action::Stop(work) = engine.handle(stop, fixture.at).action else {
                panic!("authenticated STOP must persist its intent");
            };
            let survivors = Vec::from([published.owner.connection(), connection(2, 1)]);
            (
                work.pending().record(),
                survivors,
                RetirementReason::Stopped,
            )
        } else {
            let survivors = Vec::from([connection(2, 1)]);
            let reason = RetirementReason::OwnerDisconnected;
            (published.record(), survivors, reason)
        };

        let (mut woken_engine, action, outcomes) =
            wake(&fixture, stored, &survivors, &[snapshot], fixture.at);
        assert_eq!(retired(&outcomes[0]).reason, reason);
        if stopped {
            assert!(matches!(action, Action::Stop(StopWork::Retire(_))));
            continue;
        }
        let Action::Detached(grace) = action else {
            panic!("a lost owner starts grace: {action:?}");
        };
        assert_eq!(grace.lost(), published.owner);
        // Grace keeps the lost ownership's epoch for its successor.
        let resumed = fixture.resume(&mut woken_engine, connection(1, 2));
        let Action::Resumed(resumed) = resumed.action else {
            panic!("the sender resumes its lost route");
        };
        assert_eq!(resumed.replaced, None);
        assert_eq!(resumed.registration.owner.epoch().get().get(), 2);
    }
}

#[test]
fn snapshots_that_contradict_what_survived_retire_alone() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let sender = published.owner.connection();
    let mut healthy = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    activate(&engine, &mut healthy, fixture.at);
    let healthy = healthy.snapshot().unwrap();
    let base = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at)
        .snapshot()
        .unwrap();
    let deadline = base.deadline().unwrap();
    let beyond_budget = UnixMillis(deadline.0 + 1).0.to_be_bytes();
    let invalid = RetirementReason::InvalidSnapshot;
    let cases = [
        (
            edited(base, RELAY, &[9; 32]),
            fixture.at,
            invalid(SnapshotFault::ForeignLifecycle),
        ),
        (
            edited(base, ADMITTED_BY, woken().as_bytes()),
            fixture.at,
            invalid(SnapshotFault::UnissuedAdmission),
        ),
        (
            edited(base, RECEIVER, &sender.encode()),
            fixture.at,
            invalid(SnapshotFault::InvalidReceiver),
        ),
        (
            edited(base, INSTANCE, &[9; 16]),
            fixture.at,
            invalid(SnapshotFault::InstanceMismatch),
        ),
        (
            edited(base, DEADLINE, &beyond_budget),
            fixture.at,
            invalid(SnapshotFault::DeadlineBeyondBudget),
        ),
        (
            edited(base, EPOCH, &2_u64.to_be_bytes()),
            fixture.at,
            RetirementReason::OwnerReplaced,
        ),
        (base, deadline, RetirementReason::AdmissionExpired),
    ];
    let survivors = [sender, connection(2, 1), connection(3, 1)];
    for (snapshot, at, reason) in cases {
        let (_, _, outcomes) = wake(
            &fixture,
            published.record(),
            &survivors,
            &[healthy, snapshot],
            at,
        );
        let [first, second] = <[_; 2]>::try_from(outcomes).unwrap();
        assert_eq!(restored(first).key(), healthy.key());
        let retirement = retired(&second);
        assert_eq!(
            (retirement.session, retirement.reason),
            (snapshot.key(), reason)
        );
    }

    // Two snapshots claiming one wire ID or one admission are both untrusted.
    let reused_wire = join(&fixture, &mut engine, connection(4, 1), 2, fixture.at)
        .snapshot()
        .unwrap();
    let reused_admission = edited(base, WIRE_ID, &9_u64.to_be_bytes());
    for sibling in [reused_wire, reused_admission] {
        let (_, _, outcomes) = wake(
            &fixture,
            published.record(),
            &[sender, connection(2, 1), connection(3, 1), connection(4, 1)],
            &[healthy, base, sibling],
            fixture.at,
        );
        assert!(matches!(outcomes[0], SessionRestore::Restored(_)));
        for outcome in &outcomes[1..] {
            assert_eq!(retired(outcome).reason, invalid(SnapshotFault::Duplicate));
        }
    }
}

#[test]
fn snapshots_record_only_live_admissions_in_their_exact_layout() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let deadline = session.deadline();
    let awaiting_receiver = session.snapshot().unwrap();
    let receiver = session.key().receiver();
    forward(&engine, &mut session, receiver, fixture.at);
    let awaiting_sender = session.snapshot().unwrap();
    activate(&engine, &mut session, fixture.at);
    let active = session.snapshot().unwrap();
    for (snapshot, phase, recorded_deadline, length) in [
        (
            awaiting_receiver,
            SessionPhase::AwaitingReceiver,
            deadline,
            MAX_SNAPSHOT_BYTES,
        ),
        (
            awaiting_sender,
            SessionPhase::AwaitingSender,
            deadline,
            MAX_SNAPSHOT_BYTES,
        ),
        (
            active,
            SessionPhase::Active,
            None,
            MAX_SNAPSHOT_BYTES - DEADLINE_BYTES,
        ),
    ] {
        assert_eq!(snapshot.key(), session.key());
        assert_eq!(
            (snapshot.phase(), snapshot.deadline()),
            (phase, recorded_deadline)
        );
        let encoded = snapshot.encode();
        let raw = encoded.as_bytes();
        assert_eq!(raw.len(), length);
        assert_eq!(SessionSnapshot::decode(raw), Ok(snapshot));
        for truncated in 0..raw.len() {
            assert!(SessionSnapshot::decode(&raw[..truncated]).is_err());
        }
        assert_eq!(
            SessionSnapshot::decode(&[raw, &[0]].concat()),
            Err(RecordError::Length)
        );
    }

    let raw = awaiting_receiver.encode().as_bytes().to_vec();
    let edit = |range: Range<usize>, value: u8| {
        let mut edited = raw.clone();
        edited[range].fill(value);
        SessionSnapshot::decode(&edited)
    };
    assert_eq!(edit(0..1, 2), Err(RecordError::Format));
    assert_eq!(edit(1..2, 0), Err(RecordError::Kind));
    assert_eq!(edit(1..2, 4), Err(RecordError::Kind));
    // Activity drops the deadline, so an active phase over these bytes is too long.
    assert_eq!(edit(1..2, 3), Err(RecordError::Length));
    for zeroed in [
        RELAY,
        INSTANCE,
        WIRE_ID,
        EPOCH,
        SENDER,
        ADMITTED_BY,
        RECEIVER,
        SEQUENCE,
    ] {
        assert_eq!(edit(zeroed, 0), Err(RecordError::Field));
    }

    let cancelled = SessionEvent::End {
        session: session.key(),
        participant: session.key().receiver(),
        reason: EndReason::Cancelled,
    };
    let _ = session.handle(&engine, cancelled, fixture.at, &mut ());
    assert_eq!(session.snapshot(), None);
}
