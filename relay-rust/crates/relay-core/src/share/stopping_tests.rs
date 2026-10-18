use super::*;
use crate::{error_frame::ErrorFrame, flow::Phase as FlowPhase, lifecycle::Phase};

const ONE_FRAME: Counts = Counts::new(1, 21);
const TWO_FRAMES: Counts = Counts::new(2, 42);

fn acknowledged(turn: &Turn, target: ConnectionRef) -> bool {
    turn.outputs.iter().any(|output| {
        matches!(output,
            Output::Send { connection, control: Control::Stopped(_) } if *connection == target
        )
    })
}

fn discard_queues(relay: &mut Relay, turn: &Turn, at: UnixMillis) -> Work {
    let mut commit = None;
    for output in &turn.outputs {
        if let Output::Discard(flow) = output {
            let discarded = relay.handle(Input::Discarded(*flow), at);
            assert_eq!(discarded.refusal, None);
            assert!(!acknowledged(&discarded, connection(99)));
            for output in discarded.outputs {
                if let Output::Work(work) = output {
                    assert!(commit.replace(work).is_none());
                }
            }
        }
    }
    let commit = commit.expect("queue discard completes the STOP barrier");
    assert!(matches!(next_record(&commit), Record::Stopped(_)));
    commit
}

fn submit_frames(relay: &mut Relay, key: SessionKey, flow: FlowKey, frames: u64, at: UnixMillis) {
    for ordinal in 0..frames {
        if ordinal > 0 {
            assert_eq!(
                relay
                    .handle(
                        Input::Grant {
                            flow,
                            credit: ONE_FRAME
                        },
                        at
                    )
                    .refusal,
                None
            );
        }
        let receipt = receive(relay, key, at);
        assert_eq!(
            relay.submit(&receipt, at, |_| Submission::Complete).refusal,
            None
        );
    }
}

fn stop_with_occupancy(fixture: &Fixture) -> (Relay, SessionKey, FlowKey, Work) {
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    submit_frames(&mut relay, key, flows[0], 1, fixture.at);
    let intent = work(&fixture.stop(&mut relay));
    let cleanup = complete(&mut relay, &intent, fixture.at);
    let commit = discard_queues(&mut relay, &cleanup, fixture.at);
    let stopped = complete(&mut relay, &commit, fixture.at);
    assert!(acknowledged(&stopped, connection(99)));
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
    (relay, key, flows[0], commit)
}

#[test]
fn missing_delivery_records_cannot_erase_permanent_stop() {
    let fixture = Fixture::new();
    let (original, key, _, commit) = stop_with_occupancy(&fixture);
    let checkpoint = Checkpoint::from_connections(
        &original.checkpoint_manifest().unwrap(),
        Vec::new(),
        original.limits,
    )
    .unwrap();
    let (mut recovered, _) = Relay::restore(
        fixture.identity(2),
        original.limits,
        &Restore::restarted(
            Some(next_record(&commit)),
            Responsibility::Checkpoint(&checkpoint),
        ),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(recovered.lifecycle.phase(), Phase::Stopped);
    assert_eq!(
        recovered.unavailable_reservation(key.sender()),
        Some(original.limits.delivery.total())
    );
    assert_eq!(
        recovered
            .handle(Input::Connected(connection(5)), fixture.at)
            .refusal,
        None
    );
    let turn = fixture.control(&mut recovered, connection(5), "joinB64", fixture.at);
    assert!(turn.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Error(ErrorFrame::Stopped),
            ..
        }
    )));
    assert!(recovered.sessions.is_empty());
}

#[test]
fn stopped_retains_submitted_occupancy_until_independent_evidence() {
    enum Evidence {
        Consumed,
        Settled,
        Released,
    }
    let fixture = Fixture::new();
    for evidence in [Evidence::Consumed, Evidence::Settled, Evidence::Released] {
        let mut relay = fixture.relay();
        fixture.prepare_stop(&mut relay);
        fixture.register(&mut relay);
        let (key, flows) = fixture.join(&mut relay, 2);
        submit_frames(&mut relay, key, flows[0], 2, fixture.at);
        let intent = work(&fixture.stop(&mut relay));
        let cleanup = complete(&mut relay, &intent, fixture.at);
        let commit = discard_queues(&mut relay, &cleanup, fixture.at);
        let stopped = complete(&mut relay, &commit, fixture.at);
        assert!(acknowledged(&stopped, connection(99)));
        assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
        let snapshot = relay.ledgers[&key.sender()].snapshot(flows[0]).unwrap();
        assert_eq!(snapshot.phase, FlowPhase::Settling);
        assert_eq!(snapshot.usage.submitted, TWO_FRAMES);
        assert!(relay.observer().0.iter().any(|observation| matches!(
            observation,
            Observation::Relay {
                decision: RelayDecision::StopAcknowledged {
                    unsettled_generations: 1,
                    ..
                },
                ..
            }
        )));
        assert!(
            relay
                .handle(
                    Input::Grant {
                        flow: flows[0],
                        credit: ONE_FRAME
                    },
                    fixture.at
                )
                .refusal
                .is_some()
        );
        let deadline = snapshot.deadline.unwrap();
        let _ = relay.handle(Input::Tick, deadline);
        assert_eq!(relay.ledgers[&key.sender()].usage().submitted, TWO_FRAMES);

        // Even after STOP and deadline expiry, only newly consumed bytes refund.
        let consumed = crate::flow::DeliveryConsumed::new(key.wire_id(), ONE_FRAME)
            .unwrap()
            .encode();
        for _ in 0..2 {
            assert_eq!(
                relay
                    .handle(
                        Input::Control {
                            connection: key.sender(),
                            frame: &consumed
                        },
                        deadline
                    )
                    .refusal,
                None
            );
            assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
        }
        let input = match evidence {
            Evidence::Consumed => Input::Consumed {
                connection: key.sender(),
                session: key.wire_id(),
                total: TWO_FRAMES,
            },
            Evidence::Settled => Input::Settled {
                connection: key.sender(),
                session: key.wire_id(),
                total: TWO_FRAMES,
            },
            Evidence::Released => Input::Closed(key.sender()),
        };
        assert_eq!(relay.handle(input, deadline).refusal, None);
        assert!(
            relay
                .ledgers
                .get(&key.sender())
                .is_none_or(|ledger| ledger.usage().submitted == Counts::ZERO)
        );
        assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
        assert!(
            relay
                .handle(
                    Input::Settled {
                        connection: key.sender(),
                        session: key.wire_id(),
                        total: TWO_FRAMES
                    },
                    deadline
                )
                .outputs
                .is_empty()
        );
    }
}

#[test]
fn stopped_response_connection_can_be_checkpointed_until_disconnected() {
    let fixture = Fixture::new();
    let (relay, key, _, commit) = stop_with_occupancy(&fixture);
    let checkpoint = relay.checkpoint().unwrap();
    let (surviving, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(next_record(&commit))),
            surviving: &[key.sender(), key.receiver(), connection(99)],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert_eq!(surviving.lifecycle.phase(), Phase::Stopped);
    assert_eq!(
        surviving.ledgers[&key.sender()].usage().submitted,
        ONE_FRAME
    );
}

#[test]
fn acknowledged_stop_restores_charge_and_deadline_without_sockets() {
    let fixture = Fixture::new();
    let (mut relay, key, flow, commit) = stop_with_occupancy(&fixture);
    let deadline = relay.ledgers[&key.sender()]
        .snapshot(flow)
        .unwrap()
        .deadline;
    let _ = relay.handle(Input::Disconnected(key.sender()), fixture.at);
    for participant in [key.receiver(), connection(99)] {
        let _ = relay.handle(Input::Closed(participant), fixture.at);
    }
    let mut checkpoint =
        Checkpoint::decode(&relay.checkpoint().unwrap().encode(), Fixture::limits()).unwrap();
    let limits = Limits {
        business_connections: 1,
        ..Fixture::limits()
    };
    for incarnation in [2, 3] {
        let (mut restored, _) = Relay::restore(
            fixture.identity(incarnation),
            limits,
            &Restore::restarted(
                Some(next_record(&commit)),
                Responsibility::Checkpoint(&checkpoint),
            ),
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        assert_eq!(restored.lifecycle.phase(), Phase::Stopped);
        assert_eq!(
            restored.ledgers[&key.sender()]
                .snapshot(flow)
                .unwrap()
                .deadline,
            deadline
        );
        assert_eq!(restored.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
        assert_business_capacity_refused(&fixture, &mut restored, connection(200), fixture.at);
        assert!(
            complete(&mut restored, &commit, fixture.at)
                .outputs
                .is_empty()
        );
        checkpoint = Checkpoint::decode(&restored.checkpoint().unwrap().encode(), limits).unwrap();
        relay = restored;
    }
    let late = deadline.unwrap();
    let _ = relay.handle(Input::Tick, late);
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
    let _ = relay.handle(
        Input::Settled {
            connection: key.sender(),
            session: key.wire_id(),
            total: ONE_FRAME,
        },
        late,
    );
    let _ = relay.handle(Input::Closed(key.sender()), late);
    assert_eq!(
        relay
            .handle(Input::Connected(connection(200)), late)
            .refusal,
        None
    );
    let refused = fixture.control(&mut relay, connection(200), "registerInitB64", late);
    assert!(refused.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Error(ErrorFrame::Stopped),
            ..
        }
    )));
    assert!(matches!(
        Relay::restore(
            fixture.identity(4),
            limits,
            &Restore::restarted(Some(next_record(&commit)), Responsibility::Unknown),
            Trace::default(),
            late,
        ),
        Err(Error::MissingResponsibility)
    ));
}

#[test]
fn duplicate_stop_and_uncertain_commit_do_not_settle_or_reopen_the_share() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    let intent = work(&fixture.stop(&mut relay));
    fixture.prepare_stop_on(&mut relay, connection(100));
    let duplicate = fixture.control(&mut relay, connection(100), "stopProofB64", fixture.at);
    assert!(!acknowledged(&duplicate, connection(100)));
    assert!(duplicate.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Error(ErrorFrame::Admission(_)),
            ..
        }
    )));
    let cleanup = complete(&mut relay, &intent, fixture.at);
    let commit = discard_queues(&mut relay, &cleanup, fixture.at);
    let uncertain = relay.handle(
        Input::Completed {
            id: commit.id,
            result: WorkResult::Uncertain,
        },
        fixture.at,
    );
    assert!(matches!(uncertain.outputs.as_slice(), [Output::Retry(id)] if *id == commit.id));
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
    assert_eq!(
        work(&relay.handle(Input::Retry(commit.id), fixture.at)),
        commit
    );
    assert!(acknowledged(
        &complete(&mut relay, &commit, fixture.at),
        connection(99)
    ));
    fixture.prepare_stop_on(&mut relay, connection(101));
    let repeated = fixture.control(&mut relay, connection(101), "stopProofB64", fixture.at);
    assert!(acknowledged(&repeated, connection(101)));
    assert!(
        !repeated
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
    assert!(
        relay
            .submit(&receipt, fixture.at, |_| panic!(
                "stopped receipt submitted"
            ))
            .refusal
            .is_some()
    );
    let _ = relay.handle(Input::Discarded(flows[0]), fixture.at);
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
}

#[test]
fn stop_from_grace_waits_for_old_generation_queues_without_refunding_submission() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.prepare_stop(&mut relay);
    fixture.register(&mut relay);
    let (key, flows) = fixture.join(&mut relay, 2);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    let queued = receive_from(&mut relay, key, key.sender(), fixture.at);
    let detached = relay.handle(Input::Disconnected(key.sender()), fixture.at);
    let _ = complete(&mut relay, &work(&detached), fixture.at);
    let intent = work(&fixture.stop(&mut relay));
    assert!(
        matches!(next_record(&intent), Record::Stopping(pending) if pending.cleanup() == crate::lifecycle::StopCleanup::None)
    );
    let stopping = complete(&mut relay, &intent, fixture.at);
    assert!(
        !stopping
            .outputs
            .iter()
            .any(|output| matches!(output, Output::Work(_)))
    );
    assert!(
        relay
            .submit(&queued, fixture.at, |_| panic!("old queue revived"))
            .refusal
            .is_some()
    );
    let _ = relay.handle(Input::Discarded(flows[0]), fixture.at);
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
    let commit = work(&relay.handle(Input::Discarded(flows[1]), fixture.at));
    assert!(acknowledged(
        &complete(&mut relay, &commit, fixture.at),
        connection(99)
    ));
    assert_eq!(relay.ledgers[&key.sender()].usage().submitted, ONE_FRAME);
}

#[path = "admission_tests.rs"]
mod admission_limits;
