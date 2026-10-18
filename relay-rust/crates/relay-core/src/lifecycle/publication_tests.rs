use super::*;

#[test]
fn every_event_expires_pending_ownership_before_accepting_success_or_new_work() {
    let fixture = Fixture::new();
    for trigger in 0..4 {
        let mut engine = fixture.engine();
        let commit = fixture.publish(&mut engine);
        let event = match trigger {
            0 => Event::Tick,
            1 => Event::CheckJoin(fixture.share()),
            2 => fixture.reserve_event(commit.deadline()),
            _ => Event::OwnershipCommitted(commit.id()),
        };
        let effects = engine.handle(event, commit.deadline());
        let Some(Expiration::Detached(grace)) = effects.expiration else {
            panic!("expired owner must enter grace");
        };
        assert_eq!(grace.lost(), commit.registration().owner);
        assert_eq!(engine.phase(), Phase::Grace);
        assert_eq!(
            engine
                .handle(Event::OwnershipCommitted(commit.id()), commit.deadline())
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(
            fixture.join(&mut engine, commit.deadline()).action,
            Action::Refused(Refusal::SenderDetached)
        );
    }
}

#[test]
fn duplicate_completion_or_handshake_cancellation_cannot_undo_a_published_registration() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let commit = fixture.publish(&mut engine);
    let published = committed_registration(&mut engine, commit.id(), fixture.at);
    for event in [
        Event::OwnershipCommitted(commit.id()),
        Event::Cancel {
            id: commit.id(),
            reason: Cancellation::HandshakeFailed,
        },
    ] {
        assert_eq!(
            engine.handle(event, fixture.at).action,
            Action::Refused(Refusal::StaleOperation)
        );
    }
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::JoinReady(published)
    );
}

#[test]
fn callbacks_cannot_cross_actor_incarnations_or_relays() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let commit = fixture.publish(&mut original);
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
            Restoration::restarted(None),
            incarnation,
            Recorder::default(),
            fixture.at,
            |_, _| {},
        )
        .unwrap();
        let reservation = fixture.reserve(&mut replacement, connection(1, 1), fixture.at);
        assert_eq!(reservation.id().sequence(), commit.id().sequence());
        assert_ne!(reservation.id(), commit.id());
        assert_eq!(
            replacement
                .handle(Event::OwnershipCommitted(commit.id()), fixture.at)
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(replacement.phase(), Phase::Reserved);
        let observation = replacement.observer.0.last().unwrap();
        assert_eq!(observation.operation, Some(commit.id()));
        assert_eq!(
            observation.decision,
            Decision::Refused(Refusal::StaleOperation)
        );
        assert_eq!(observation.relay, relay);
        assert_eq!(observation.incarnation, incarnation);
    }
}

#[test]
fn a_callback_cannot_skip_authentication_and_the_registration_request() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    assert_eq!(
        engine
            .handle(Event::OwnershipCommitted(reservation.id()), fixture.at)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(engine.phase(), Phase::Reserved);
}
