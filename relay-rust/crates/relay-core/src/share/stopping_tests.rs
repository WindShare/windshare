use super::*;
use crate::{error_frame::ErrorFrame, flow::Phase as FlowPhase, lifecycle::Phase};

const ONE_FRAME: Counts = Counts::new(1, 21);
const TWO_FRAMES: Counts = Counts::new(2, 42);

fn acknowledged(step: &RelayStep, target: ConnectionRef) -> bool {
    step.outputs.iter().any(|output| {
        matches!(output,
            Output::Send { connection, control: Control::Stopped(_) } if *connection == target
        )
    })
}

fn discard_queues(relay: &mut Relay, step: &RelayStep, at: UnixMillis) -> Work {
    let mut commit = None;
    for output in &step.outputs {
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
        let sender = key.sender();
        submit_frames(&mut relay, key, flows[0], 2, fixture.at);
        let intent = work(&fixture.stop(&mut relay));
        let cleanup = complete(&mut relay, &intent, fixture.at);
        let commit = discard_queues(&mut relay, &cleanup, fixture.at);
        let stopped = complete(&mut relay, &commit, fixture.at);
        assert!(acknowledged(&stopped, connection(99)));
        assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
        let snapshot = ledger_of(&relay, sender).snapshot(flows[0]).unwrap();
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
        assert_eq!(ledger_of(&relay, sender).usage().submitted, TWO_FRAMES);

        // Even after STOP and deadline expiry, only newly consumed bytes refund.
        let consumed = crate::flow::DeliveryConsumed::new(key.wire_id(), ONE_FRAME)
            .unwrap()
            .encode();
        for _ in 0..2 {
            assert_eq!(
                relay
                    .handle(
                        Input::Control {
                            connection: sender,
                            frame: &consumed
                        },
                        deadline
                    )
                    .refusal,
                None
            );
            assert_eq!(ledger_of(&relay, sender).usage().submitted, ONE_FRAME);
        }
        let input = match evidence {
            Evidence::Consumed => Input::Consumed {
                connection: sender,
                session: key.wire_id(),
                total: TWO_FRAMES,
            },
            Evidence::Settled => Input::Settled {
                connection: sender,
                session: key.wire_id(),
                total: TWO_FRAMES,
            },
            Evidence::Released => Input::Closed(sender),
        };
        assert_eq!(relay.handle(input, deadline).refusal, None);
        assert!(
            relay
                .generations
                .ledger(&sender)
                .is_none_or(|ledger| ledger.usage().submitted == Counts::ZERO)
        );
        assert_eq!(relay.lifecycle.phase(), Phase::Stopped);
        assert!(
            relay
                .handle(
                    Input::Settled {
                        connection: sender,
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
    assert_eq!(ledger_of(&relay, key.sender()).usage().submitted, ONE_FRAME);
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
    assert_eq!(ledger_of(&relay, key.sender()).usage().submitted, ONE_FRAME);
    assert!(
        relay
            .submit(&receipt, fixture.at, |_| panic!(
                "stopped receipt submitted"
            ))
            .refusal
            .is_some()
    );
    let _ = relay.handle(Input::Discarded(flows[0]), fixture.at);
    assert_eq!(ledger_of(&relay, key.sender()).usage().submitted, ONE_FRAME);
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
    assert_eq!(ledger_of(&relay, key.sender()).usage().submitted, ONE_FRAME);
    let commit = work(&relay.handle(Input::Discarded(flows[1]), fixture.at));
    assert!(acknowledged(
        &complete(&mut relay, &commit, fixture.at),
        connection(99)
    ));
    assert_eq!(ledger_of(&relay, key.sender()).usage().submitted, ONE_FRAME);
}

#[path = "admission_tests.rs"]
mod admission_limits;
