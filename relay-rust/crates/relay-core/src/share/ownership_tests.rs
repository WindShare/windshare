use super::*;
use crate::error_frame::ErrorFrame;

fn registered(turn: &Turn) -> bool {
    turn.outputs.iter().any(|output| {
        matches!(
            output,
            Output::Send {
                control: Control::Registered(_),
                ..
            }
        )
    })
}

fn join_error(fixture: &Fixture, relay: &mut Relay, slot: u64, at: UnixMillis) -> ErrorFrame {
    assert_eq!(
        relay.handle(Input::Connected(connection(slot)), at).refusal,
        None
    );
    let turn = fixture.control(relay, connection(slot), "joinB64", at);
    assert!(!turn.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Descriptor { .. },
            ..
        }
    )));
    turn.outputs
        .iter()
        .find_map(|output| match output {
            Output::Send {
                connection: target,
                control: Control::Error(frame),
            } if *target == connection(slot) => Some(*frame),
            _ => None,
        })
        .expect("JOIN refusal")
}

#[test]
fn join_preserves_absent_and_permanently_stopped_outcomes() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    assert_eq!(
        join_error(&fixture, &mut relay, 2, fixture.at),
        ErrorFrame::NotFound
    );
    fixture.prepare_stop(&mut relay);
    let intent = work(&fixture.stop(&mut relay));
    let commit = work(&complete(&mut relay, &intent, fixture.at));
    let turn = complete(&mut relay, &commit, fixture.at);
    assert!(turn.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Stopped(_),
            ..
        }
    )));
    assert_eq!(
        join_error(&fixture, &mut relay, 3, fixture.at),
        ErrorFrame::Stopped
    );
}

#[test]
fn stop_during_uncertain_ownership_commit_never_acknowledges_registration() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    let publication = fixture.begin_registration(&mut relay);
    let ownership = work(&complete(&mut relay, &publication, fixture.at));
    let uncertain = relay.handle(
        Input::Completed {
            id: ownership.id,
            result: WorkResult::Uncertain,
        },
        fixture.at,
    );
    assert!(matches!(uncertain.outputs.as_slice(), [Output::Retry(id)] if *id == ownership.id));
    let stopping = fixture.stop(&mut relay);
    assert!(
        !stopping
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_) | Output::Close(_)))
    );
    let retry = work(&relay.handle(Input::Retry(ownership.id), fixture.at));
    assert_eq!(retry, ownership);
    let completed = complete(&mut relay, &ownership, fixture.at);
    assert!(!registered(&completed));
    let intent = work(&completed);
    let cleanup = complete(&mut relay, &intent, fixture.at);
    assert!(
        cleanup
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == connection(1)))
    );
    let commit = work(&cleanup);
    let stopped = complete(&mut relay, &commit, fixture.at);
    assert!(stopped.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Stopped(_),
            ..
        }
    )));
    assert!(
        complete(&mut relay, &ownership, fixture.at)
            .outputs
            .is_empty()
    );
    assert_eq!(
        join_error(&fixture, &mut relay, 2, fixture.at),
        ErrorFrame::Stopped
    );
}

#[test]
fn resume_revokes_old_handoffs_immediately_but_admits_new_sessions_only_after_commit() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let at = fixture.begin_resume(&mut relay, 3);
    let pending = fixture.control(&mut relay, connection(3), "resumeProofB64", at);
    assert!(!registered(&pending));
    let ownership = work(&pending);
    let mut handed_off = false;
    let refused = relay.submit(&receipt, at, |_| {
        handed_off = true;
        Submission::Complete
    });
    assert!(refused.refusal.is_some());
    assert!(!handed_off);
    assert!(matches!(
        join_error(&fixture, &mut relay, 4, at),
        ErrorFrame::Starting(_)
    ));
    let uncertain = relay.handle(
        Input::Completed {
            id: ownership.id,
            result: WorkResult::Uncertain,
        },
        at,
    );
    assert!(matches!(uncertain.outputs.as_slice(), [Output::Retry(id)] if *id == ownership.id));
    assert_eq!(
        work(&relay.handle(Input::Retry(ownership.id), at)),
        ownership
    );
    assert!(registered(&complete(&mut relay, &ownership, at)));
    assert_eq!(
        relay.handle(Input::Connected(connection(5)), at).refusal,
        None
    );
    let joined = fixture.control(&mut relay, connection(5), "joinB64", at);
    assert!(joined.outputs.iter().any(|output| matches!(output, Output::Send { control: Control::Descriptor { session, .. }, .. } if session.sender() == connection(3))));
}

#[test]
fn pending_resume_disconnect_and_timeout_preserve_grace_and_close_the_replaced_owner() {
    let fixture = Fixture::new();
    for timeout in [false, true] {
        let mut relay = fixture.relay();
        fixture.register(&mut relay);
        let at = fixture.begin_resume(&mut relay, 3);
        let ownership = work(&fixture.control(&mut relay, connection(3), "resumeProofB64", at));
        let at = if timeout {
            UnixMillis(at.0 + REGISTRATION_BUDGET)
        } else {
            let _ = relay.handle(Input::Disconnected(connection(3)), at);
            at
        };
        let completed = complete(&mut relay, &ownership, at);
        assert!(!registered(&completed));
        assert!(
            completed
                .outputs
                .iter()
                .any(|output| matches!(output, Output::Close(value) if *value == connection(1)))
        );
        let grace = work(&completed);
        let Record::Grace(record) = next_record(&grace) else {
            panic!("pending owner loss must be persisted");
        };
        assert_eq!(record.lost().connection(), connection(3));
        assert!(!registered(&complete(&mut relay, &grace, at)));
    }
}
