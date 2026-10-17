use super::*;

fn restarted() -> Incarnation {
    Incarnation::new([2; 16]).unwrap()
}

fn finish(
    engine: &mut ShareLifecycle<Recorder>,
    id: OperationId,
    result: PublicationResult,
    at: UnixMillis,
) -> Action<'static> {
    engine
        .handle(Event::PublicationFinished { id, result }, at)
        .action
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
fn a_published_route_re_adopts_its_surviving_ownership_without_a_write() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let publication = fixture.publish(&mut original).id;
    let Action::CommitOwnership(commit) = finish(
        &mut original,
        publication,
        PublicationResult::Committed,
        fixture.at,
    ) else {
        panic!("publication must request the ownership commit");
    };
    let published = committed_registration(&mut original, commit.id(), fixture.at);
    let owner = published.owner.connection();
    let (mut engine, action) = fixture
        .restore_at(
            Some(published.record()),
            &[connection(7, 1), owner],
            restarted(),
            fixture.at,
        )
        .unwrap();
    assert_eq!(action, Action::None);
    assert_eq!(engine.phase(), Phase::Published);
    assert_eq!(engine.deadline(), None);

    let Action::JoinReady(adopted) = fixture.join(&mut engine, fixture.at).action else {
        panic!("a surviving owner keeps serving receivers");
    };
    // Rebuilding the actor is no ownership change: sessions stay bound to it.
    assert_eq!(adopted, published);
    assert_eq!(adopted.owner.epoch(), OwnershipEpoch::FIRST);
    let restored = engine.observer.0[0];
    assert_eq!(
        (restored.ownership, restored.connection, restored.after),
        (Some(published.owner), Some(owner), Phase::Published)
    );
    assert_eq!(restored.incarnation, restarted());

    // Callbacks for the operation that published it name no current work,
    // while the re-adopted owner can still be lost.
    assert_eq!(
        finish(
            &mut engine,
            publication,
            PublicationResult::Aborted,
            fixture.at
        ),
        Action::Refused(Refusal::StaleOperation)
    );
    assert!(matches!(
        engine
            .handle(Event::OwnerDisconnected(owner), fixture.at)
            .action,
        Action::Detached(_)
    ));
}

#[test]
fn a_lost_owner_derives_one_grace_that_later_restores_never_extend() {
    let fixture = Fixture::new();
    let published = fixture.register(&mut fixture.engine());
    let owner = published.owner.connection();
    let (mut engine, action) = fixture
        .restore_at(
            Some(published.record()),
            &[connection(7, 1)],
            restarted(),
            fixture.at,
        )
        .unwrap();
    let deadline = fixture.at.saturating_add(SENDER_GRACE);
    let grace = GraceRoute::new(published.route, published.owner, deadline);
    assert_eq!(action, Action::Detached(grace));
    assert_eq!(grace.lost(), published.owner);
    assert_eq!(engine.phase(), Phase::Grace);
    assert_eq!(engine.deadline(), Some(deadline));
    let restored = engine.observer.0[0];
    assert_eq!(
        (restored.connection, restored.decision),
        (Some(owner), Decision::Detached)
    );
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

    // The host stored the derived deadline. Restoring it later keeps it, and
    // a socket that outlived its own loss is not re-adopted from a grace.
    let later = UnixMillis(deadline.0 - 1);
    let (mut engine, action) = fixture
        .restore_at(
            Some(grace.record()),
            &[owner],
            Incarnation::new([3; 16]).unwrap(),
            later,
        )
        .unwrap();
    assert_eq!(action, Action::None);
    assert_eq!(engine.phase(), Phase::Grace);
    assert_eq!(engine.deadline(), Some(deadline));
    assert_eq!(
        engine.handle(Event::Tick, deadline).expiration,
        Some(Expiration::Lapsed(published.route))
    );

    // A grace restored after its deadline lapses on the first event.
    let much_later = deadline.saturating_add(Duration::from_secs(600));
    let (mut engine, _) = fixture
        .restore_at(
            Some(grace.record()),
            &[],
            Incarnation::new([4; 16]).unwrap(),
            much_later,
        )
        .unwrap();
    assert_eq!(engine.deadline(), Some(deadline));
    let effects = fixture.join(&mut engine, much_later);
    assert_eq!(
        effects.expiration,
        Some(Expiration::Lapsed(published.route))
    );
    assert_eq!(effects.action, Action::Refused(Refusal::NotFound));
}

#[test]
fn a_stop_record_retires_its_instance_and_keeps_the_share_occupied() {
    let fixture = Fixture::new();
    let (mut engine, action) = fixture
        .restore(Some(fixture.stopped()), restarted())
        .unwrap();
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

    let mut other_instance = bytes(&fixture.vector.register_init_b64);
    other_instance[INIT_INSTANCE_OFFSET] ^= 1;
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
        let restored = ShareLifecycle::restore(
            relay,
            share,
            Restoration::restarted(stored),
            incarnation,
            (),
            fixture.at,
            |_, _| {},
        )
        .err();
        assert_eq!(restored, Some(error));
    }
    let published = fixture.register(&mut fixture.engine());
    let grace = GraceRoute::new(published.route, published.owner, fixture.at);
    for record in [published.record(), grace.record(), fixture.stopped()] {
        let restored = ShareLifecycle::restore(
            fixture.relay(),
            other_share,
            Restoration::restarted(Some(record)),
            restarted(),
            (),
            fixture.at,
            |_, _| {},
        )
        .err();
        assert_eq!(restored, Some(RestoreError::ForeignShare));
    }
}
