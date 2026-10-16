use super::*;

fn resumed(effects: Effects<'static>) -> Resumed {
    assert_eq!(effects.expiration, None);
    match effects.action {
        Action::Resumed(resumed) => resumed,
        action => panic!("expected a committed resume: {action:?}"),
    }
}

#[test]
fn losing_the_owner_starts_one_grace_and_its_lapse_reopens_the_share() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let owner = published.id.connection();
    let lost_at = fixture.at;
    // A late close of another lifetime of the owner's slot names no owner.
    assert_eq!(
        engine
            .handle(Event::OwnerDisconnected(connection(1, 2)), lost_at)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(engine.phase(), Phase::Published);

    let deadline = lost_at.saturating_add(SENDER_GRACE);
    let grace = GraceRoute {
        route: published.route,
        deadline,
    };
    assert_eq!(
        engine
            .handle(Event::OwnerDisconnected(owner), lost_at)
            .action,
        Action::Detached(grace)
    );
    assert_eq!(grace.record(), Record::Grace(grace));
    assert_eq!(engine.phase(), Phase::Grace);
    assert_eq!(engine.deadline(), Some(deadline));
    assert_eq!(
        fixture.join(&mut engine, lost_at).action,
        Action::Refused(Refusal::SenderDetached)
    );
    assert_eq!(
        engine
            .handle(fixture.reserve_event(lost_at), lost_at)
            .action,
        Action::Refused(Refusal::Occupied)
    );
    assert_eq!(
        engine
            .handle(Event::OwnerDisconnected(owner), lost_at)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );

    assert_eq!(
        engine
            .handle(Event::Tick, UnixMillis(deadline.0 - 1))
            .expiration,
        None
    );
    let effects = fixture.join(&mut engine, deadline);
    assert_eq!(
        effects.expiration,
        Some(Expiration::Lapsed(published.route))
    );
    assert_eq!(effects.action, Action::Refused(Refusal::NotFound));
    assert_eq!(engine.phase(), Phase::Vacant);
    assert_eq!(engine.deadline(), None);
    assert!(matches!(
        engine
            .handle(fixture.reserve_event(deadline), deadline)
            .action,
        Action::Reserved(_)
    ));

    let observations = &engine.observer.0;
    let detached = observations
        .iter()
        .find(|observation| observation.decision == Decision::Detached)
        .unwrap();
    assert_eq!(
        (detached.event, detached.connection, detached.before),
        (
            ObservedEvent::OwnerDisconnected,
            Some(owner),
            Phase::Published
        )
    );
    let lapsed = observations
        .iter()
        .find(|observation| observation.decision == Decision::Lapsed)
        .unwrap();
    assert_eq!(
        (lapsed.event, lapsed.at, lapsed.before, lapsed.after),
        (
            ObservedEvent::Deadline,
            deadline,
            Phase::Grace,
            Phase::Vacant
        )
    );
}

#[test]
fn resume_takes_over_a_live_or_lost_owner_and_answers_like_a_publication() {
    let fixture = Fixture::new();
    for owner_lost in [false, true] {
        let mut engine = fixture.engine();
        let published = fixture.register(&mut engine);
        let owner = published.id.connection();
        if owner_lost {
            assert!(matches!(
                engine
                    .handle(Event::OwnerDisconnected(owner), fixture.at)
                    .action,
                Action::Detached(_)
            ));
        }
        let successor = connection(2, 1);
        let committed = resumed(fixture.resume(&mut engine, successor));
        assert_eq!(committed.replaced, (!owner_lost).then_some(owner));
        let registration = committed.registration;
        assert_eq!(registration.id.connection(), successor);
        assert_eq!(registration.route, published.route);
        assert_eq!(
            registration.record(),
            Record::Published(OwnedRoute {
                route: published.route,
                owner: successor,
            })
        );
        assert_eq!(
            registration.acknowledgement().encode().as_slice(),
            bytes(&fixture.vector.registered_b64)
        );
        assert_eq!(engine.phase(), Phase::Published);
        assert_eq!(engine.deadline(), None);
        assert_eq!(
            fixture.join(&mut engine, fixture.resume_at).action,
            Action::JoinReady(registration)
        );
        // The replaced owner's late close no longer names the owner.
        assert_eq!(
            engine
                .handle(Event::OwnerDisconnected(owner), fixture.resume_at)
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        let observation = engine
            .observer
            .0
            .iter()
            .find(|observation| observation.decision == Decision::Resumed)
            .unwrap();
        assert_eq!(
            (observation.operation, observation.connection),
            (Some(registration.id), Some(successor))
        );
    }
}

#[test]
fn a_resume_commits_only_over_the_ownership_it_observed() {
    let fixture = Fixture::new();
    let at = fixture.resume_at;

    // The observed owner leaving mid-handshake leaves nobody to protect.
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let attempt = fixture.begin_resume(&mut engine, connection(2, 1));
    assert!(matches!(
        engine
            .handle(Event::OwnerDisconnected(published.id.connection()), at)
            .action,
        Action::Detached(_)
    ));
    let resumption = fixture.authenticate_resume(attempt);
    assert_eq!(
        resumed(engine.handle(Event::Resume(resumption), at)).replaced,
        None
    );

    // Of two resumes that observed one ownership, only the first commits.
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let first = fixture.begin_resume(&mut engine, connection(2, 1));
    let second = fixture.begin_resume(&mut engine, connection(3, 1));
    let second = fixture.authenticate_resume(second);
    let winner = resumed(engine.handle(Event::Resume(fixture.authenticate_resume(first)), at));
    assert_eq!(
        engine.handle(Event::Resume(second), at).action,
        Action::Refused(Refusal::ResumeStale)
    );
    assert_eq!(
        fixture.join(&mut engine, at).action,
        Action::JoinReady(winner.registration)
    );

    // A resume admitted before the route existed cannot replace its owner.
    let mut engine = fixture.engine();
    let Action::ResumeAdmitted(early) = engine
        .handle(fixture.resume_event(connection(2, 1)), fixture.at)
        .action
    else {
        panic!("an absent route still admits an authenticated resume");
    };
    let published = fixture.register(&mut engine);
    assert_eq!(
        engine
            .handle(Event::Resume(fixture.authenticate_resume(early)), at)
            .action,
        Action::Refused(Refusal::ResumeStale)
    );
    assert_eq!(
        fixture.join(&mut engine, at).action,
        Action::JoinReady(published)
    );

    // A restore replaces every ownership an earlier incarnation observed.
    let mut original = fixture.engine();
    let published = fixture.register(&mut original);
    let attempt = fixture.begin_resume(&mut original, connection(2, 1));
    let (mut restored, _) = fixture
        .restore_at(
            Some(published.record()),
            &[published.id.connection()],
            Incarnation::new([2; 16]).unwrap(),
            at,
        )
        .unwrap();
    assert_eq!(
        restored
            .handle(Event::Resume(fixture.authenticate_resume(attempt)), at)
            .action,
        Action::Refused(Refusal::ResumeStale)
    );
    assert_eq!(restored.phase(), Phase::Published);
}

#[test]
fn an_authenticated_resume_learns_whether_any_route_remains() {
    let fixture = Fixture::new();
    let at = fixture.resume_at;
    let published = fixture.register(&mut fixture.engine());

    let mut vacant = fixture.engine();
    assert_eq!(
        fixture.resume(&mut vacant, connection(2, 1)).action,
        Action::Refused(Refusal::NotFound)
    );

    // The grace lapses between admission and commit.
    let grace = GraceRoute {
        route: published.route,
        deadline: UnixMillis(at.0 + 1),
    };
    let (mut engine, _) = fixture
        .restore_at(
            Some(grace.record()),
            &[],
            Incarnation::new([2; 16]).unwrap(),
            at,
        )
        .unwrap();
    let resumption =
        fixture.authenticate_resume(fixture.begin_resume(&mut engine, connection(2, 1)));
    let effects = engine.handle(Event::Resume(resumption), grace.deadline);
    assert_eq!(
        effects.expiration,
        Some(Expiration::Lapsed(published.route))
    );
    assert_eq!(effects.action, Action::Refused(Refusal::NotFound));

    // Commits respect the handshake deadline and the actor they belong to.
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let attempt = fixture.begin_resume(&mut engine, connection(2, 1));
    let deadline = attempt.deadline();
    let resumption = fixture.authenticate_resume(attempt);
    let (mut foreign, _) = ShareLifecycle::restore(
        fixture.relay(),
        ShareId::new([0x5a; 12]).unwrap(),
        None,
        [],
        Incarnation::new([1; 16]).unwrap(),
        (),
        at,
    )
    .unwrap();
    let late = engine.handle(Event::Resume(resumption), deadline);
    assert_eq!(late.action, Action::Refused(Refusal::Expired));
    let resumption =
        fixture.authenticate_resume(fixture.begin_resume(&mut engine, connection(3, 1)));
    assert_eq!(
        foreign.handle(Event::Resume(resumption), at).action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(engine.phase(), Phase::Published);
}

#[test]
fn resume_admission_refuses_before_a_challenge_when_it_can_never_commit() {
    let fixture = Fixture::new();
    let at = fixture.resume_at;
    let begin = |engine: &mut ShareLifecycle<Recorder>, init: RegisterInit| {
        let event = Event::BeginResume {
            connection: connection(2, 1),
            init,
            deadline: at.saturating_add(Duration::from_millis(VECTOR_REGISTRATION_MILLIS)),
        };
        engine.handle(event, at).action
    };

    let mut engine = fixture.engine();
    assert_eq!(
        begin(&mut engine, fixture.init()),
        Action::Refused(Refusal::UnsupportedMode)
    );
    assert_eq!(
        engine
            .handle(
                fixture.resume_event(connection(2, 1)),
                at.saturating_add(Duration::from_millis(VECTOR_REGISTRATION_MILLIS))
            )
            .action,
        Action::Refused(Refusal::Expired)
    );
    let (mut wrong_actor, _) = ShareLifecycle::restore(
        fixture.relay(),
        ShareId::new([0x5a; 12]).unwrap(),
        None,
        [],
        Incarnation::new([1; 16]).unwrap(),
        Recorder::default(),
        at,
    )
    .unwrap();
    assert_eq!(
        begin(&mut wrong_actor, fixture.resume_init()),
        Action::Refused(Refusal::WrongShare)
    );
    assert_eq!(engine.last_sequence, 0);

    // A publication in progress or in doubt makes the resume wait.
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    assert_eq!(
        begin(&mut engine, fixture.resume_init()),
        Action::Refused(Refusal::Starting)
    );
    let Action::Publish(request) = engine
        .handle(
            Event::Publish(fixture.authenticate(reservation)),
            fixture.at,
        )
        .action
    else {
        panic!("authenticated upload must request a host transaction");
    };
    assert_eq!(
        begin(&mut engine, fixture.foreign_resume_init()),
        Action::Refused(Refusal::ResumeMismatch)
    );
    assert_eq!(
        begin(&mut engine, fixture.resume_init()),
        Action::Refused(Refusal::Starting)
    );
    let uncertain = Event::PublicationFinished {
        id: request.id,
        result: PublicationResult::Uncertain,
    };
    assert_eq!(
        engine.handle(uncertain, fixture.at).action,
        Action::Reconcile(request.id)
    );
    assert_eq!(
        begin(&mut engine, fixture.resume_init()),
        Action::Refused(Refusal::PublicationUncertain)
    );

    // Another registration's route or a stopped instance never resumes.
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    assert_eq!(
        begin(&mut engine, fixture.foreign_resume_init()),
        Action::Refused(Refusal::ResumeMismatch)
    );
    let (mut stopped, _) = fixture
        .restore(Some(fixture.stopped()), Incarnation::new([2; 16]).unwrap())
        .unwrap();
    assert_eq!(
        begin(&mut stopped, fixture.resume_init()),
        Action::Refused(Refusal::Stopped)
    );
    assert_eq!(
        begin(&mut stopped, fixture.foreign_resume_init()),
        Action::Refused(Refusal::ResumeMismatch)
    );
    assert_eq!(stopped.phase(), Phase::Stopped);
}
