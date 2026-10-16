use super::*;
use crate::identifiers::{ShareInstance, StopId};

fn restarted() -> Incarnation {
    Incarnation::new([2; 16]).unwrap()
}

#[test]
fn every_interrupted_stop_stage_restores_cleanup_without_re_adopting_survivors() {
    let fixture = Fixture::new();
    for interrupted in [StopStep::Persist, StopStep::Retire, StopStep::Commit] {
        for survives in [false, true] {
            let mut original = fixture.engine();
            let published = fixture.register(&mut original);
            let mut work = stop(&fixture, &mut original, fixture.at);
            while work.step() != interrupted {
                work = advance(&mut original, work, fixture.at);
            }
            // The intent write reached storage, but any subsequent callback or
            // final write may have been lost. No stage stores completion early.
            let pending = work.pending();
            let owner = published.owner.connection();
            let surviving = if survives { &[owner][..] } else { &[] };
            let (mut engine, action) = fixture
                .restore_at(Some(pending.record()), surviving, restarted(), fixture.at)
                .unwrap();
            let retire = StopWork::Retire(pending);
            assert_eq!(action, Action::Stop(retire));
            assert_eq!(pending.cleanup(), StopCleanup::Owner(owner));
            assert_fenced(&fixture, &mut engine, fixture.at);
            let observation = engine.observer.0[0];
            assert_eq!(observation.operation, Some(pending.id()));
            assert_eq!(observation.connection, Some(pending.id().connection()));
            assert_eq!(observation.incarnation, restarted());
            for old in [StopWork::Persist(pending), StopWork::Commit(pending)] {
                assert_eq!(
                    finish(&mut engine, old, StopResult::Complete, fixture.at),
                    Action::Refused(Refusal::StaleOperation)
                );
            }
            complete(&mut engine, retire, fixture.at);
            assert_eq!(engine.phase(), Phase::Stopped);
        }
    }
}

#[test]
fn final_record_restores_stopped_even_when_its_commit_callback_was_lost() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    fixture.register(&mut original);
    let persist = stop(&fixture, &mut original, fixture.at);
    let retire = advance(&mut original, persist, fixture.at);
    let commit = advance(&mut original, retire, fixture.at);
    // Host wrote the final record before crashing, without returning completion.
    let stored = commit.pending().stopped().record();
    let (mut engine, action) = fixture.restore(Some(stored), restarted()).unwrap();
    assert_eq!(action, Action::None);
    assert_eq!(engine.phase(), Phase::Stopped);
    assert_eq!(
        finish(&mut engine, commit, StopResult::Complete, fixture.at),
        Action::Refused(Refusal::StaleOperation)
    );
    let retry_connection = connection(2, 2);
    let Action::Stopped(ack) = engine
        .handle(stop_event(&fixture, retry_connection), fixture.at)
        .action
    else {
        panic!("lost STOPPED is recovered by authenticated retry");
    };
    assert_eq!(ack.connection(), retry_connection);
    assert_eq!(engine.deadline(), None);
    assert_eq!(
        engine
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::Stopped)
    );
}

#[test]
fn stored_stop_preserves_publication_identity_and_cannot_restore_with_either_old_incarnation() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let publication = fixture.publish(&mut original);
    // A new incarnation first reconciles the unknown publication, then STOP
    // adopts its exact cleanup obligation without erasing the old operation.
    let (mut reconciler, _) = fixture
        .restore(Some(publication.record()), restarted())
        .unwrap();
    let work = stop(&fixture, &mut reconciler, fixture.at);
    let pending = work.pending();
    assert_ne!(pending.id().incarnation(), publication.id.incarnation());
    assert_eq!(pending.cleanup(), StopCleanup::Publication(publication.id));
    for incarnation in [pending.id().incarnation(), publication.id.incarnation()] {
        assert_eq!(
            fixture.restore(Some(pending.record()), incarnation).err(),
            Some(RestoreError::ReusedIncarnation)
        );
    }
    let (mut engine, action) = fixture
        .restore(Some(pending.record()), Incarnation::new([3; 16]).unwrap())
        .unwrap();
    assert_eq!(action, Action::Stop(StopWork::Retire(pending)));
    assert_eq!(
        engine
            .handle(
                Event::PublicationFinished {
                    id: publication.id,
                    result: PublicationResult::Aborted,
                },
                fixture.at
            )
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    complete(&mut engine, StopWork::Retire(pending), fixture.at);
}

#[test]
fn stop_callbacks_cannot_name_another_actor_connection_generation_or_operation() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let work = stop(&fixture, &mut engine, fixture.at);
    let mut foreign = fixture.engine();
    foreign.incarnation = restarted();
    let old = stop(&fixture, &mut foreign, fixture.at);
    let mut another_generation = work.pending();
    another_generation.id.connection.generation = NonZeroU64::new(2).unwrap();
    let mut another_sequence = work.pending();
    another_sequence.id.sequence = NonZeroU64::new(2).unwrap();
    for stale in [
        old,
        StopWork::Persist(another_generation),
        StopWork::Persist(another_sequence),
    ] {
        assert_eq!(
            finish(&mut engine, stale, StopResult::Complete, fixture.at),
            Action::Refused(Refusal::StaleOperation)
        );
    }
    assert_fenced(&fixture, &mut engine, fixture.at);
}

#[test]
fn authenticated_stop_must_match_actor_and_cannot_replace_another_authenticated_instance() {
    let fixture = Fixture::new();
    for wrong_relay in [false, true] {
        let (relay, share, refusal) = if wrong_relay {
            (
                RelayIdentity::new([9; 32]).unwrap(),
                fixture.share(),
                Refusal::WrongRelay,
            )
        } else {
            (
                fixture.relay(),
                ShareId::new([9; 12]).unwrap(),
                Refusal::WrongShare,
            )
        };
        let (mut engine, _) = ShareLifecycle::restore(
            relay,
            share,
            Restoration::restarted(None),
            restarted(),
            Recorder::default(),
            fixture.at,
            |_, _| {},
        )
        .unwrap();
        assert_eq!(
            engine
                .handle(stop_event(&fixture, connection(2, 1)), fixture.at)
                .action,
            Action::Refused(refusal)
        );
        assert_eq!(engine.phase(), Phase::Vacant);
    }

    let registration = fixture.init();
    let other = Registration::new(
        registration.registration().route(),
        ShareInstance::new([9; 16]).unwrap(),
        registration.registration().descriptor_digest(),
        registration.registration().resume_token_hash(),
    );
    let publication = fixture.publish(&mut fixture.engine()).id;
    let published = fixture.register(&mut fixture.engine());
    let route = PublishedRoute {
        relay: fixture.relay(),
        registration: other,
    };
    let owner = route.ownership(published.owner.epoch(), published.owner.connection());
    for stored in [
        Record::Published(OwnedRoute {
            route,
            epoch: owner.epoch(),
            owner: owner.connection(),
        }),
        Record::Grace(GraceRoute::new(
            route,
            owner,
            fixture.at.saturating_add(SENDER_GRACE),
        )),
        Record::Unresolved(UnresolvedPublication {
            id: publication,
            registration: other,
        }),
    ] {
        let (mut engine, _) = fixture.restore(Some(stored), restarted()).unwrap();
        let before = engine.phase();
        assert_eq!(
            engine
                .handle(stop_event(&fixture, connection(2, 1)), fixture.at)
                .action,
            Action::Refused(Refusal::StopMismatch)
        );
        assert_eq!(engine.phase(), before);
    }
}

#[test]
fn a_refused_stop_still_expires_the_incumbent_and_sequence_exhaustion_never_wraps() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let deadline = reservation.deadline();
    engine.relay = RelayIdentity::new([9; 32]).unwrap();
    let effects = engine.handle(stop_event(&fixture, connection(2, 1)), deadline);
    assert_eq!(effects.action, Action::Refused(Refusal::WrongRelay));
    assert_eq!(
        effects.expiration,
        Some(Expiration::Released(reservation.id()))
    );
    assert_eq!(engine.phase(), Phase::Vacant);

    let mut engine = fixture.engine();
    engine.last_sequence = u64::MAX;
    assert_eq!(
        engine
            .handle(stop_event(&fixture, connection(2, 1)), fixture.at)
            .action,
        Action::Refused(Refusal::SequenceExhausted)
    );
    assert_eq!(engine.phase(), Phase::Vacant);
    assert_eq!(engine.last_sequence, u64::MAX);
}

#[test]
fn stop_retry_cannot_rewrite_a_stopping_or_stopped_identity() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let pending = stop(&fixture, &mut original, fixture.at).pending();
    for other_instance in [false, true] {
        let mut other = pending;
        let reason = if other_instance {
            other.share_instance = ShareInstance::new([9; 16]).unwrap();
            Refusal::StopMismatch
        } else {
            other.stop_id = StopId::new([9; 16]).unwrap();
            Refusal::Stopped
        };
        for record in [other.record(), other.stopped().record()] {
            let (mut engine, _) = fixture.restore(Some(record), restarted()).unwrap();
            let before = engine.phase();
            assert_eq!(
                engine
                    .handle(stop_event(&fixture, connection(2, 1)), fixture.at)
                    .action,
                Action::Refused(reason)
            );
            assert_eq!(engine.phase(), before);
        }
    }
}
