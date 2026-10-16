use super::*;
use crate::stop::StopAuthority;

// The shared STOP challenge uses the handshake's ten-second budget.
const VECTOR_STOP_MILLIS: u64 = 10_000;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct StopVector {
    stop_challenge_id_b64: String,
    stop_challenge_nonce_b64: String,
    stop_expires_at: Decimal,
    stop_proof_b64: String,
    stopped_b64: String,
}

pub(super) fn authority(fixture: &Fixture, connection: ConnectionRef) -> StopAuthority {
    let vector: StopVector = session_case("fresh-relay-registration-proof");
    let at = UnixMillis(unix_seconds(&vector.stop_expires_at).start().0 - VECTOR_STOP_MILLIS - 1);
    let mut handshake = ControlHandshake::open(fixture.relay(), connection, at);
    let init = bytes(&fixture.vector.stop_init_b64);
    assert_eq!(
        handshake.receive(&init, at),
        Step::RequestAdmission(AdmissionRequest::Stop(StopInit::decode(&init).unwrap()))
    );
    let seed = challenge_seed(
        &vector.stop_challenge_id_b64,
        &vector.stop_challenge_nonce_b64,
    );
    assert!(matches!(
        handshake.admit(Admission::Stop, seed, at),
        Step::SendChallenge(_)
    ));
    match handshake.receive(&bytes(&vector.stop_proof_b64), fixture.at) {
        Step::Complete(Outcome::Stop(authority)) => authority,
        step => panic!("expected authenticated STOP: {step:?}"),
    }
}

fn stop_event(fixture: &Fixture, connection: ConnectionRef) -> Event<'static> {
    Event::Stop {
        connection,
        authority: authority(fixture, connection),
    }
}

fn stop(fixture: &Fixture, engine: &mut ShareLifecycle<Recorder>, at: UnixMillis) -> StopWork {
    let effects = engine.handle(stop_event(fixture, connection(2, 1)), at);
    assert_eq!(effects.expiration, None);
    match effects.action {
        Action::Stop(work) => work,
        action => panic!("expected STOP work: {action:?}"),
    }
}

fn finish(
    engine: &mut ShareLifecycle<Recorder>,
    work: StopWork,
    result: StopResult,
    at: UnixMillis,
) -> Action<'static> {
    let effects = engine.handle(Event::StopFinished { work, result }, at);
    assert_eq!(effects.expiration, None);
    effects.action
}

fn advance(engine: &mut ShareLifecycle<Recorder>, work: StopWork, at: UnixMillis) -> StopWork {
    match finish(engine, work, StopResult::Complete, at) {
        Action::Stop(work) => work,
        action => panic!("expected next STOP phase: {action:?}"),
    }
}

fn complete(
    engine: &mut ShareLifecycle<Recorder>,
    mut work: StopWork,
    at: UnixMillis,
) -> StopAcknowledgement {
    loop {
        match finish(engine, work, StopResult::Complete, at) {
            Action::Stop(next) => work = next,
            Action::Stopped(ack) => return ack,
            action => panic!("expected STOP progress: {action:?}"),
        }
    }
}

fn assert_fenced(fixture: &Fixture, engine: &mut ShareLifecycle<Recorder>, at: UnixMillis) {
    assert_eq!(engine.phase(), Phase::Stopping);
    assert_eq!(engine.deadline(), None);
    for event in [
        Event::CheckJoin(fixture.share()),
        fixture.reserve_event(at),
        Event::BeginResume {
            connection: connection(4, 1),
            init: fixture.resume_init(),
            deadline: at.saturating_add(Duration::from_secs(5)),
        },
    ] {
        assert_eq!(
            engine.handle(event, at).action,
            Action::Refused(Refusal::StopPending)
        );
    }
}

#[test]
fn stopped_waits_for_recording_exact_retirement_and_final_durability() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let work = stop(&fixture, &mut engine, fixture.at);
    let pending = work.pending();
    assert_eq!(work.step(), StopStep::Persist);
    assert_eq!(
        pending.cleanup(),
        StopCleanup::Owner(published.owner.connection())
    );
    assert_eq!(pending.id().connection(), connection(2, 1));
    assert_eq!(pending.stopped().record(), fixture.stopped());
    assert_fenced(&fixture, &mut engine, fixture.at);

    // Even an otherwise authentic completion cannot skip a prerequisite phase.
    for premature in [StopWork::Retire(pending), StopWork::Commit(pending)] {
        assert_eq!(
            finish(&mut engine, premature, StopResult::Complete, fixture.at),
            Action::Refused(Refusal::StaleOperation)
        );
    }
    let stored = Record::decode(pending.record().encode().as_bytes()).unwrap();
    assert_eq!(stored, Record::Stopping(pending));
    let retire = advance(&mut engine, work, fixture.at);
    assert_eq!(retire, StopWork::Retire(pending));
    assert_eq!(
        finish(&mut engine, work, StopResult::Complete, fixture.at),
        Action::Refused(Refusal::StaleOperation)
    );
    let commit = advance(&mut engine, retire, fixture.at);
    assert_eq!(commit, StopWork::Commit(pending));
    assert_fenced(&fixture, &mut engine, fixture.at);
    let ack = complete(&mut engine, commit, fixture.at);
    let vector: StopVector = session_case("fresh-relay-registration-proof");
    assert_eq!(ack.connection(), connection(2, 1));
    assert_eq!(
        ack.acknowledgement().encode().as_slice(),
        bytes(&vector.stopped_b64)
    );
    assert_eq!(engine.phase(), Phase::Stopped);
    assert_eq!(
        finish(&mut engine, commit, StopResult::Complete, fixture.at),
        Action::Refused(Refusal::StaleOperation)
    );
}

#[test]
fn uncertainty_at_each_phase_keeps_the_same_irreversible_obligation() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut work = stop(&fixture, &mut engine, fixture.at);
    let later = fixture.at.saturating_add(Duration::from_secs(600));
    for step in [StopStep::Persist, StopStep::Retire, StopStep::Commit] {
        assert_eq!(work.step(), step);
        for _ in 0..2 {
            assert_eq!(
                finish(&mut engine, work, StopResult::Uncertain, later),
                Action::Stop(work)
            );
            assert_fenced(&fixture, &mut engine, later);
            assert_eq!(
                engine.handle(Event::Tick, later),
                Effects {
                    revocation: None,
                    expiration: None,
                    action: Action::None
                }
            );
            assert_eq!(
                engine
                    .handle(Event::OwnerDisconnected(connection(2, 1)), later)
                    .action,
                Action::Refused(Refusal::StaleOperation)
            );
        }
        if step != StopStep::Commit {
            work = advance(&mut engine, work, later);
        }
    }
    complete(&mut engine, work, later);
    assert_eq!(
        fixture.join(&mut engine, later).action,
        Action::Refused(Refusal::Stopped)
    );
}

#[test]
fn stop_owns_expired_reservation_cleanup_and_rejects_delayed_uploads() {
    let fixture = Fixture::new();
    for at_deadline in [false, true] {
        let mut engine = fixture.engine();
        let reservation = fixture.reserve(&mut engine, connection(1, 3), fixture.at);
        let id = reservation.id();
        let at = if at_deadline {
            reservation.deadline()
        } else {
            fixture.at
        };
        let upload = fixture.authenticate(reservation);
        let work = stop(&fixture, &mut engine, at);
        assert_eq!(work.pending().cleanup(), StopCleanup::Reservation(id));
        assert_eq!(
            engine.handle(Event::Publish(upload), at).action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id,
                        reason: Cancellation::Disconnected
                    },
                    at
                )
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        complete(&mut engine, work, at);
        assert_eq!(engine.phase(), Phase::Stopped);
    }
}

#[test]
fn a_reservation_cannot_claim_another_instance_to_block_authenticated_stop() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let mut init = bytes(&fixture.vector.register_init_b64);
    init[INIT_INSTANCE_OFFSET] ^= 1;
    let Action::Reserved(reservation) = engine
        .handle(
            Event::Reserve {
                connection: connection(1, 1),
                init: RegisterInit::decode(&init).unwrap(),
                deadline: fixture.at.saturating_add(Duration::from_secs(5)),
            },
            fixture.at,
        )
        .action
    else {
        panic!("unauthenticated reservation");
    };
    let work = stop(&fixture, &mut engine, fixture.at);
    assert_eq!(
        work.pending().cleanup(),
        StopCleanup::Reservation(reservation.id())
    );
    complete(&mut engine, work, fixture.at);
}

#[test]
fn stop_subsumes_publication_cleanup_and_late_results_never_release_its_fence() {
    let fixture = Fixture::new();
    for fenced in [false, true] {
        for at_deadline in [false, true] {
            let mut engine = fixture.engine();
            let request = fixture.publish(&mut engine);
            if fenced {
                assert_eq!(
                    engine
                        .handle(
                            Event::PublicationFinished {
                                id: request.id,
                                result: PublicationResult::Uncertain,
                            },
                            fixture.at
                        )
                        .action,
                    Action::Reconcile(request.id)
                );
            }
            let at = if at_deadline {
                request.deadline
            } else {
                fixture.at
            };
            let work = stop(&fixture, &mut engine, at);
            assert_eq!(
                work.pending().cleanup(),
                StopCleanup::Publication(request.id)
            );
            for completed in [false, true] {
                if completed {
                    complete(&mut engine, work, at);
                }
                for result in [
                    PublicationResult::Committed,
                    PublicationResult::Uncertain,
                    PublicationResult::Aborted,
                ] {
                    assert_eq!(
                        engine
                            .handle(
                                Event::PublicationFinished {
                                    id: request.id,
                                    result
                                },
                                at
                            )
                            .action,
                        Action::Refused(Refusal::StaleOperation)
                    );
                }
                assert_eq!(
                    engine.phase(),
                    if completed {
                        Phase::Stopped
                    } else {
                        Phase::Stopping
                    }
                );
            }
        }
    }
}

#[test]
fn absent_and_grace_routes_stop_permanently_even_at_the_grace_boundary() {
    let fixture = Fixture::new();
    for grace in [false, true] {
        let mut engine = fixture.engine();
        let at = if grace {
            let published = fixture.register(&mut engine);
            let Action::Detached(route) = engine
                .handle(
                    Event::OwnerDisconnected(published.owner.connection()),
                    fixture.at,
                )
                .action
            else {
                panic!("grace");
            };
            route.deadline()
        } else {
            fixture.at
        };
        let work = stop(&fixture, &mut engine, at);
        assert_eq!(work.pending().cleanup(), StopCleanup::None);
        complete(&mut engine, work, at);
        let later = at.saturating_add(Duration::from_secs(600));
        assert_eq!(engine.handle(Event::Tick, later).expiration, None);
        assert_eq!(
            engine.handle(fixture.reserve_event(later), later).action,
            Action::Refused(Refusal::Stopped)
        );
    }
}

#[test]
fn resume_races_retire_only_the_owner_present_when_stop_wins() {
    let fixture = Fixture::new();
    for resume_first in [false, true] {
        let mut engine = fixture.engine();
        let old = fixture.register(&mut engine);
        let new_owner = connection(1, 2);
        let attempt = fixture.begin_resume(&mut engine, new_owner);
        let resumption = fixture.authenticate_resume(attempt);
        if resume_first {
            let Action::Resumed(resumed) = engine
                .handle(Event::Resume(resumption), fixture.resume_at)
                .action
            else {
                panic!("resume wins");
            };
            assert_eq!(resumed.replaced, Some(old.owner.connection()));
            let work = stop(&fixture, &mut engine, fixture.resume_at);
            assert_eq!(work.pending().cleanup(), StopCleanup::Owner(new_owner));
            assert_eq!(
                engine
                    .handle(
                        Event::OwnerDisconnected(old.owner.connection()),
                        fixture.resume_at
                    )
                    .action,
                Action::Refused(Refusal::StaleOperation)
            );
            complete(&mut engine, work, fixture.resume_at);
        } else {
            let work = stop(&fixture, &mut engine, fixture.resume_at);
            assert_eq!(
                work.pending().cleanup(),
                StopCleanup::Owner(old.owner.connection())
            );
            assert_eq!(
                engine
                    .handle(Event::Resume(resumption), fixture.resume_at)
                    .action,
                Action::Refused(Refusal::StopPending)
            );
            complete(&mut engine, work, fixture.resume_at);
        }
        assert_eq!(
            engine
                .handle(fixture.resume_event(new_owner), fixture.resume_at)
                .action,
            Action::Refused(Refusal::Stopped)
        );
    }
}

#[test]
fn duplicate_stop_waits_without_allocating_waiters_then_acknowledges_its_exact_retry_connection() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let work = stop(&fixture, &mut engine, fixture.at);
    let sequence = engine.last_sequence;
    for generation in 2..5 {
        assert_eq!(
            engine
                .handle(stop_event(&fixture, connection(2, generation)), fixture.at)
                .action,
            Action::Refused(Refusal::StopPending)
        );
        assert_eq!(engine.last_sequence, sequence);
    }
    complete(&mut engine, work, fixture.at);
    for generation in 2..5 {
        let Action::Stopped(ack) = engine
            .handle(stop_event(&fixture, connection(2, generation)), fixture.at)
            .action
        else {
            panic!("idempotent STOP");
        };
        assert_eq!(ack.connection(), connection(2, generation));
        assert_eq!(engine.last_sequence, sequence);
    }
}

#[test]
fn observations_correlate_stop_identity_stage_uncertainty_and_exact_connections() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let begin = engine.observer.0.len();
    let persist = stop(&fixture, &mut engine, fixture.at);
    assert_eq!(
        finish(&mut engine, persist, StopResult::Uncertain, fixture.at),
        Action::Stop(persist)
    );
    complete(&mut engine, persist, fixture.at);
    let observations = &engine.observer.0[begin..];
    assert_eq!(observations.len(), 5);
    assert_eq!(
        observations[0].event,
        ObservedEvent::Stop(persist.pending().stopped().stop_id())
    );
    assert_eq!(
        (observations[0].before, observations[0].after),
        (Phase::Published, Phase::Stopping)
    );
    assert_eq!(
        observations[1].event,
        ObservedEvent::StopFinished {
            step: StopStep::Persist,
            result: StopResult::Uncertain,
        }
    );
    assert_eq!(
        observations[3].decision,
        Decision::Stop {
            step: StopStep::Commit,
            cleanup: persist.pending().cleanup()
        }
    );
    assert_eq!(observations[4].decision, Decision::Stopped);
    assert_eq!(observations[4].after, Phase::Stopped);
    for observation in observations {
        assert_eq!(observation.operation, Some(persist.pending().id()));
        assert_eq!(observation.connection, Some(connection(2, 1)));
        assert_eq!(observation.relay, fixture.relay());
        assert_eq!(observation.share, fixture.share());
    }
    assert_eq!(
        persist.pending().cleanup(),
        StopCleanup::Owner(published.owner.connection())
    );
}

#[path = "stop_restoration_tests.rs"]
mod restoration;
