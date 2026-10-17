use super::*;

fn pending(
    fixture: &Fixture,
    engine: &mut ShareLifecycle<Recorder>,
    resume: bool,
) -> OwnershipCommit {
    if resume {
        fixture.register(engine);
        ownership_commit(fixture.resume(engine, connection(1, 2)))
    } else {
        let publication = fixture.publish(engine);
        ownership_commit(engine.handle(
            Event::PublicationFinished {
                id: publication.id,
                result: PublicationResult::Committed,
            },
            fixture.at,
        ))
    }
}

#[test]
fn pending_ownership_blocks_service_and_new_resumes_until_exact_confirmation() {
    let fixture = Fixture::new();
    for resume in [false, true] {
        let mut engine = fixture.engine();
        let commit = pending(&fixture, &mut engine, resume);
        let at = if resume {
            fixture.resume_at
        } else {
            fixture.at
        };
        assert_eq!(engine.phase(), Phase::Activating);
        assert_eq!(engine.deadline(), Some(commit.deadline()));
        assert_eq!(engine.session_owner(), None);
        assert_eq!(
            fixture.join(&mut engine, at).action,
            Action::Refused(Refusal::Starting)
        );
        assert_eq!(
            engine
                .handle(fixture.resume_event(connection(3, 1)), at)
                .action,
            Action::Refused(Refusal::Starting)
        );
        let registration = committed_registration(&mut engine, commit.id(), at);
        assert_eq!(engine.session_owner(), Some(registration.owner));
        let observation = engine.observer.0.last().unwrap();
        assert_eq!(observation.event, ObservedEvent::OwnershipCommitted);
        assert_eq!(observation.operation, Some(commit.id()));
        assert_eq!(
            (observation.before, observation.after),
            (Phase::Activating, Phase::Published)
        );
        assert_eq!(
            fixture.join(&mut engine, at).action,
            Action::JoinReady(registration)
        );
        assert_eq!(
            engine
                .handle(Event::OwnershipCommitted(commit.id()), at)
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
    }
}

#[test]
fn final_confirmation_checks_the_original_deadline_without_a_connection_timer() {
    let fixture = Fixture::new();
    for resume in [false, true] {
        let mut engine = fixture.engine();
        let commit = pending(&fixture, &mut engine, resume);
        let deadline = commit.deadline();
        let effects = engine.handle(Event::OwnershipCommitted(commit.id()), deadline);
        let Some(Expiration::Detached(grace)) = effects.expiration else {
            panic!("expired activation must persist owner loss");
        };
        assert_eq!(grace.lost(), commit.registration().owner);
        assert_eq!(grace.deadline(), deadline.saturating_add(SENDER_GRACE));
        assert_eq!(effects.action, Action::Refused(Refusal::StaleOperation));
        assert_eq!(engine.phase(), Phase::Grace);
        assert_eq!(
            fixture.join(&mut engine, deadline).action,
            Action::Refused(Refusal::SenderDetached)
        );
        assert_eq!(engine.handle(Event::Tick, deadline).expiration, None);
    }
}

#[test]
fn disconnect_or_cancellation_during_commit_cannot_be_undone_by_its_completion() {
    let fixture = Fixture::new();
    for resume in [false, true] {
        for cancel in [false, true] {
            let mut engine = fixture.engine();
            let commit = pending(&fixture, &mut engine, resume);
            let at = if resume {
                fixture.resume_at
            } else {
                fixture.at
            };
            let event = if cancel {
                Event::Cancel {
                    id: commit.id(),
                    reason: Cancellation::Disconnected,
                }
            } else {
                Event::OwnerDisconnected(commit.id().connection())
            };
            let Action::Detached(grace) = engine.handle(event, at).action else {
                panic!("losing a pending owner must preserve its grace");
            };
            assert_eq!(
                engine
                    .handle(Event::OwnershipCommitted(commit.id()), at)
                    .action,
                Action::Refused(Refusal::StaleOperation)
            );
            assert_eq!(engine.phase(), Phase::Grace);
            assert_eq!(engine.deadline(), Some(grace.deadline()));
        }
    }
}

#[test]
fn old_commit_cannot_activate_a_replacement_actor_or_ownership() {
    let fixture = Fixture::new();
    let mut original = fixture.engine();
    let old = pending(&fixture, &mut original, false);
    let (mut replacement, _) = fixture
        .restore(None, Incarnation::new([2; 16]).unwrap())
        .unwrap();
    let current = pending(&fixture, &mut replacement, false);
    assert_ne!(old.id(), current.id());
    assert_eq!(
        replacement
            .handle(Event::OwnershipCommitted(old.id()), fixture.at)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(replacement.phase(), Phase::Activating);
    committed_registration(&mut replacement, current.id(), fixture.at);
    let resumed = ownership_commit(fixture.resume(&mut replacement, connection(1, 2)));
    assert_eq!(
        replacement
            .handle(Event::OwnershipCommitted(current.id()), fixture.resume_at)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(replacement.phase(), Phase::Activating);
    committed_registration(&mut replacement, resumed.id(), fixture.resume_at);
}
