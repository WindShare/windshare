use super::*;

fn finish(
    engine: &mut ShareLifecycle<Recorder>,
    id: OperationId,
    result: PublicationResult,
    at: UnixMillis,
) -> Effects<'static> {
    engine.handle(Event::PublicationFinished { id, result }, at)
}

#[test]
fn confirmed_abort_releases_capacity_and_fences_all_old_host_callbacks() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let request = fixture.publish(&mut engine);
    assert_eq!(
        finish(
            &mut engine,
            request.id,
            PublicationResult::Aborted,
            fixture.at
        )
        .action,
        Action::Released(request.id)
    );
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::Refused(Refusal::NotFound)
    );
    let replacement = fixture.reserve(&mut engine, connection(1, 2), fixture.at);
    assert_ne!(replacement.id(), request.id);
    assert_eq!(replacement.id().connection().id, request.id.connection().id);
    assert_ne!(
        replacement.id().connection().generation,
        request.id.connection().generation
    );
    for result in [
        PublicationResult::Committed,
        PublicationResult::Aborted,
        PublicationResult::Uncertain,
    ] {
        assert_eq!(
            finish(&mut engine, request.id, result, fixture.at).action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(engine.phase(), Phase::Reserved);
    }
    assert!(matches!(
        engine
            .handle(
                Event::Publish(fixture.authenticate(replacement)),
                fixture.at
            )
            .action,
        Action::Publish(_)
    ));
}

#[test]
fn uncertain_publication_never_reopens_or_becomes_visible_on_late_success() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let request = fixture.publish(&mut engine);
    assert_eq!(
        finish(
            &mut engine,
            request.id,
            PublicationResult::Uncertain,
            fixture.at
        )
        .action,
        Action::Reconcile(request.id)
    );
    assert_eq!(engine.phase(), Phase::Fenced);
    assert_eq!(engine.deadline(), None);
    for result in [PublicationResult::Committed, PublicationResult::Uncertain] {
        assert_eq!(
            finish(&mut engine, request.id, result, fixture.at).action,
            Action::Reconcile(request.id)
        );
        assert_eq!(
            fixture.join(&mut engine, fixture.at).action,
            Action::Refused(Refusal::PublicationUncertain)
        );
        assert_eq!(
            engine
                .handle(fixture.reserve_event(fixture.at), fixture.at)
                .action,
            Action::Refused(Refusal::PublicationUncertain)
        );
    }
    let much_later = request.deadline.saturating_add(Duration::from_secs(600));
    assert_eq!(engine.handle(Event::Tick, much_later).expiration, None);
    assert_eq!(engine.phase(), Phase::Fenced);
    assert_eq!(
        finish(
            &mut engine,
            request.id,
            PublicationResult::Aborted,
            much_later
        )
        .action,
        Action::Released(request.id)
    );
    assert!(matches!(
        engine
            .handle(fixture.reserve_event(much_later), much_later)
            .action,
        Action::Reserved(_)
    ));
}

#[test]
fn cancellation_during_publication_waits_for_quiescent_rollback() {
    let fixture = Fixture::new();
    for reason in [
        Cancellation::Disconnected,
        Cancellation::HostFailed,
        Cancellation::HandshakeFailed,
    ] {
        let mut engine = fixture.engine();
        let request = fixture.publish(&mut engine);
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id: request.id,
                        reason
                    },
                    fixture.at
                )
                .action,
            Action::Reconcile(request.id)
        );
        assert_eq!(engine.phase(), Phase::Fenced);
        assert_eq!(
            finish(
                &mut engine,
                request.id,
                PublicationResult::Committed,
                fixture.at
            )
            .action,
            Action::Reconcile(request.id)
        );
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id: request.id,
                        reason
                    },
                    fixture.at
                )
                .action,
            Action::Reconcile(request.id)
        );
        assert_eq!(
            finish(
                &mut engine,
                request.id,
                PublicationResult::Aborted,
                fixture.at
            )
            .action,
            Action::Released(request.id)
        );
        let replacement = fixture.reserve(&mut engine, connection(1, 2), fixture.at);
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id: request.id,
                        reason
                    },
                    fixture.at
                )
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(engine.deadline(), Some(replacement.deadline()));
    }
}

#[test]
fn every_event_checks_publication_expiry_before_accepting_success_or_new_work() {
    let fixture = Fixture::new();
    for trigger in 0..4 {
        let mut engine = fixture.engine();
        let request = fixture.publish(&mut engine);
        let event = match trigger {
            0 => Event::Tick,
            1 => Event::Join(fixture.share()),
            2 => fixture.reserve_event(request.deadline),
            _ => Event::PublicationFinished {
                id: request.id,
                result: PublicationResult::Committed,
            },
        };
        let effects = engine.handle(event, request.deadline);
        if trigger == 3 {
            assert_eq!(effects.expiration, None);
            assert_eq!(effects.action, Action::Reconcile(request.id));
        } else {
            assert_eq!(effects.expiration, Some(Expiration::Reconcile(request.id)));
        }
        assert_eq!(engine.phase(), Phase::Fenced);
        assert_eq!(
            fixture.join(&mut engine, request.deadline).action,
            Action::Refused(Refusal::PublicationUncertain)
        );
        assert_eq!(
            finish(
                &mut engine,
                request.id,
                PublicationResult::Committed,
                request.deadline
            )
            .action,
            Action::Reconcile(request.id)
        );
        assert_eq!(
            finish(
                &mut engine,
                request.id,
                PublicationResult::Aborted,
                request.deadline
            )
            .action,
            Action::Released(request.id)
        );
    }
}

#[test]
fn duplicate_completion_or_handshake_cancellation_cannot_undo_a_published_registration() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let request = fixture.publish(&mut engine);
    let Action::Registered(published) = finish(
        &mut engine,
        request.id,
        PublicationResult::Committed,
        fixture.at,
    )
    .action
    else {
        panic!("first commit must publish");
    };
    for result in [
        PublicationResult::Committed,
        PublicationResult::Aborted,
        PublicationResult::Uncertain,
    ] {
        assert_eq!(
            finish(&mut engine, request.id, result, fixture.at).action,
            Action::Refused(Refusal::StaleOperation)
        );
    }
    assert_eq!(
        engine
            .handle(
                Event::Cancel {
                    id: request.id,
                    reason: Cancellation::HandshakeFailed
                },
                fixture.at
            )
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::JoinReady(published)
    );
}

#[test]
fn callbacks_cannot_cross_actor_incarnations_or_relays() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let request = fixture.publish(&mut original);
    for (relay, incarnation) in [
        (fixture.relay(), Incarnation::new([2; 16]).unwrap()),
        (
            RelayIdentity::new([9; 32]).unwrap(),
            Incarnation::new([1; 16]).unwrap(),
        ),
    ] {
        let (mut replacement, _) = ShareLifecycle::restore(
            relay,
            fixture.share(),
            None,
            [],
            incarnation,
            Recorder::default(),
            fixture.at,
        )
        .unwrap();
        let reservation = fixture.reserve(&mut replacement, connection(1, 1), fixture.at);
        assert_eq!(reservation.id().sequence(), request.id.sequence());
        assert_ne!(reservation.id(), request.id);
        assert_eq!(
            finish(
                &mut replacement,
                request.id,
                PublicationResult::Committed,
                fixture.at
            )
            .action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(replacement.phase(), Phase::Reserved);
        let observation = replacement.observer.0.last().unwrap();
        assert_eq!(observation.operation, Some(request.id));
        assert_eq!(
            observation.decision,
            Decision::Refused(Refusal::StaleOperation)
        );
        assert_eq!(observation.relay, relay);
        assert_eq!(observation.incarnation, incarnation);
    }
}

#[test]
fn a_callback_cannot_skip_authentication_and_host_publication_request() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    for result in [
        PublicationResult::Committed,
        PublicationResult::Aborted,
        PublicationResult::Uncertain,
    ] {
        assert_eq!(
            finish(&mut engine, reservation.id(), result, fixture.at).action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(engine.phase(), Phase::Reserved);
    }
}

#[test]
fn expiry_does_not_request_reconciliation_after_a_confirmed_abort() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let request = fixture.publish(&mut engine);
    let effects = finish(
        &mut engine,
        request.id,
        PublicationResult::Aborted,
        request.deadline,
    );
    assert_eq!(effects.expiration, None);
    assert_eq!(effects.action, Action::Released(request.id));
    assert_eq!(engine.phase(), Phase::Vacant);
}
