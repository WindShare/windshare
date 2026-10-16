use crate::stop::StopInit;

use super::*;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct StopVector {
    stop_init_b64: String,
}

fn restarted() -> Incarnation {
    Incarnation::new([2; 16]).unwrap()
}

fn finish(
    engine: &mut ShareLifecycle<Recorder>,
    id: RegistrationId,
    result: PublicationResult,
    at: UnixMillis,
) -> Action<'static> {
    engine
        .handle(Event::PublicationFinished { id, result }, at)
        .action
}

fn register(fixture: &Fixture, engine: &mut ShareLifecycle<Recorder>) -> PublishedRegistration {
    let request = fixture.publish(engine);
    match finish(engine, request.id, PublicationResult::Committed, fixture.at) {
        Action::Registered(published) => published,
        action => panic!("publication must register: {action:?}"),
    }
}

fn stopped() -> Record {
    let vector: StopVector = session_case("fresh-relay-registration-proof");
    let stop = StopInit::decode(&bytes(&vector.stop_init_b64)).unwrap();
    Record::Stopped(StoppedShare {
        relay: stop.relay(),
        route: stop.route(),
        share_instance: stop.share_instance(),
        stop_id: stop.stop_id(),
    })
}

#[test]
fn a_stored_publication_restores_fenced_and_only_its_exact_abort_reopens_the_share() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let request = fixture.publish(&mut original);
    let stored = request.record();
    assert_eq!(stored.relay(), fixture.relay());
    assert_eq!(stored.share(), fixture.share());

    // Restart before the transaction reports, long after the handshake deadline.
    let much_later = request.deadline.saturating_add(Duration::from_secs(600));
    let (mut engine, action) = fixture.restore(Some(stored), restarted()).unwrap();
    assert_eq!(action, Action::Reconcile(request.id));
    assert_eq!(engine.phase(), Phase::Fenced);
    assert_eq!(engine.deadline(), None);
    let restored = engine.observer.0[0];
    assert_eq!(restored.event, ObservedEvent::Restore);
    assert_eq!(restored.operation, Some(request.id));
    assert_eq!(restored.incarnation, restarted());
    assert_eq!(restored.decision, Decision::Reconcile);

    assert_eq!(engine.handle(Event::Tick, much_later).expiration, None);
    for result in [PublicationResult::Committed, PublicationResult::Uncertain] {
        assert_eq!(
            finish(&mut engine, request.id, result, much_later),
            Action::Reconcile(request.id)
        );
    }
    assert_eq!(
        fixture.join(&mut engine, much_later).action,
        Action::Refused(Refusal::PublicationUncertain)
    );
    assert_eq!(
        engine
            .handle(fixture.reserve_event(much_later), much_later)
            .action,
        Action::Refused(Refusal::PublicationUncertain)
    );
    assert_eq!(
        finish(
            &mut engine,
            request.id,
            PublicationResult::Aborted,
            much_later
        ),
        Action::Released(request.id)
    );
    let Action::Reserved(replacement) = engine
        .handle(fixture.reserve_event(much_later), much_later)
        .action
    else {
        panic!("a confirmed abort reopens the share");
    };
    assert_eq!(replacement.id().incarnation(), restarted());
    assert_ne!(replacement.id(), request.id);
}

#[test]
fn a_registered_route_restores_detached_with_its_version_across_restarts() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let published = register(&fixture, &mut original);
    assert_eq!(published.route.version(), RouteVersion::FIRST);
    assert_eq!(
        published.route.registration(),
        fixture.init().registration()
    );

    let mut stored = published.record();
    for incarnation in [restarted(), Incarnation::new([3; 16]).unwrap()] {
        let (mut engine, action) = fixture.restore(Some(stored), incarnation).unwrap();
        assert_eq!(action, Action::None);
        assert!(matches!(engine.state, State::Detached(route) if route == published.route));
        assert_eq!(engine.deadline(), None);
        assert_eq!(
            fixture.join(&mut engine, fixture.at).action,
            Action::Refused(Refusal::SenderDetached)
        );
        assert_eq!(
            engine
                .handle(fixture.reserve_event(fixture.at), fixture.at)
                .action,
            Action::Refused(Refusal::Occupied)
        );
        // Callbacks for the operation that published it name no current work.
        assert_eq!(
            finish(
                &mut engine,
                published.id,
                PublicationResult::Aborted,
                fixture.at
            ),
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(engine.phase(), Phase::Detached);
        let State::Detached(route) = engine.state else {
            unreachable!();
        };
        stored = Record::Published(route);
    }
}

#[test]
fn a_stop_record_retires_its_instance_and_keeps_the_share_occupied() {
    let fixture = Fixture::new();
    let (mut engine, action) = fixture.restore(Some(stopped()), restarted()).unwrap();
    assert_eq!(action, Action::None);
    assert_eq!(engine.phase(), Phase::Stopped);
    assert_eq!(engine.deadline(), None);
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::Refused(Refusal::Stopped)
    );
    assert_eq!(
        engine
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::Stopped)
    );

    // WS2R carries the instance right after the eight-byte header and share ID.
    let mut other_instance = bytes(&fixture.vector.register_init_b64);
    other_instance[8 + 12] ^= 1;
    let other_instance = RegisterInit::decode(&other_instance).unwrap();
    assert_ne!(
        other_instance.registration().share_instance(),
        fixture.init().registration().share_instance()
    );
    assert_eq!(
        engine
            .handle(
                Event::Reserve {
                    connection: connection(1, 1),
                    init: other_instance,
                    deadline: fixture
                        .at
                        .saturating_add(Duration::from_millis(VECTOR_REGISTRATION_MILLIS)),
                },
                fixture.at
            )
            .action,
        Action::Refused(Refusal::Occupied)
    );
    assert_eq!(engine.phase(), Phase::Stopped);
}

#[test]
fn restoration_refuses_records_of_another_relay_or_share_and_a_reused_incarnation() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let request = fixture.publish(&mut original);
    let stored = Some(request.record());
    let other_share = ShareId::new([0x5a; 12]).unwrap();
    for (relay, share, incarnation, error) in [
        (
            RelayIdentity::new([9; 32]).unwrap(),
            fixture.share(),
            restarted(),
            RestoreError::ForeignRelay,
        ),
        (
            fixture.relay(),
            other_share,
            restarted(),
            RestoreError::ForeignShare,
        ),
        (
            fixture.relay(),
            fixture.share(),
            request.id.incarnation(),
            RestoreError::ReusedIncarnation,
        ),
    ] {
        let restored =
            ShareLifecycle::restore(relay, share, stored, incarnation, (), fixture.at).err();
        assert_eq!(restored, Some(error));
    }
    for record in [
        Record::Published(register(&fixture, &mut fixture.engine()).route),
        stopped(),
    ] {
        let restored = ShareLifecycle::restore(
            fixture.relay(),
            other_share,
            Some(record),
            restarted(),
            (),
            fixture.at,
        )
        .err();
        assert_eq!(restored, Some(RestoreError::ForeignShare));
    }
}
