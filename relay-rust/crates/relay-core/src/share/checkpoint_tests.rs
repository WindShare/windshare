use super::*;
use std::{collections::BTreeMap, vec};

const ATTACHMENT_BYTES: usize = 16_384;
const HOST_ENVELOPE_BYTES: usize = 512;
const PERSONAL_SESSION_LIMIT: usize = 32;

fn budget() -> CheckpointBudget {
    CheckpointBudget::new(ATTACHMENT_BYTES, HOST_ENVELOPE_BYTES).unwrap()
}

#[derive(Default)]
struct Attachments {
    bytes: BTreeMap<ConnectionRef, Vec<u8>>,
    written: Vec<ConnectionRef>,
}

impl Attachments {
    fn flush(&mut self, relay: &mut Relay, now: UnixMillis) -> CheckpointFlush {
        self.written.clear();
        let result = relay
            .flush_checkpoints(now, |connection, bytes| {
                assert!(bytes.len() + HOST_ENVELOPE_BYTES <= ATTACHMENT_BYTES);
                self.bytes.insert(connection, bytes.to_vec());
                self.written.push(connection);
                Ok::<_, ()>(())
            })
            .unwrap();
        let checkpoint = self.checkpoint();
        assert_eq!(checkpoint.encode(), relay.checkpoint().unwrap().encode());
        result
    }

    fn checkpoint(&self) -> Checkpoint {
        Checkpoint::from_connections(
            self.bytes
                .values()
                .map(|bytes| ConnectionCheckpoint::decode(bytes, Fixture::limits()).unwrap())
                .collect(),
            Fixture::limits(),
        )
        .unwrap()
    }

    fn restore(&self, fixture: &Fixture, stored: Record) -> Relay {
        let checkpoint = self.checkpoint();
        let (relay, turn) = Relay::restore(
            fixture.identity(2),
            Fixture::limits(),
            &Restore {
                record: Some(stored),
                surviving: &self.bytes.keys().copied().collect::<Vec<_>>(),
                checkpoint: Some(&checkpoint),
            },
            Trace::default(),
            fixture.at,
        )
        .unwrap();
        assert!(turn.refusal.is_none());
        relay
    }
}

fn active(relay: &mut Relay, fixture: &Fixture, receiver: u64) -> (SessionKey, [FlowKey; 2]) {
    let (key, flows) = fixture.join(relay, receiver);
    let receipt = receive(relay, key, fixture.at);
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    activate(relay, key, fixture.at);
    (key, flows)
}

#[test]
fn native_hosts_do_not_allocate_an_attachment_journal() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    active(&mut relay, &fixture, 2);
    assert!(matches!(
        relay.checkpointing,
        checkpoint_tracking::Checkpointing::Disabled
    ));
    assert_eq!(
        relay.flush_checkpoints(fixture.at, |_, _| Ok::<_, ()>(())),
        Err(CheckpointWriteError::State(Error::CheckpointDisabled))
    );
}

#[test]
fn data_and_feedback_update_only_the_destination_and_replay_writes_nothing() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (first, flows) = active(&mut relay, &fixture, 2);
    let (sibling, _) = active(&mut relay, &fixture, 3);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    assert_eq!(host.flush(&mut relay, fixture.at).connections, 3);
    let sibling_bytes = host.bytes[&sibling.receiver()].clone();
    let consumed = Input::Consumed {
        connection: first.sender(),
        session: first.wire_id(),
        total: Counts::new(1, 21),
    };
    assert_eq!(relay.handle(consumed, fixture.at).refusal, None);
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [first.sender()]);
    assert_eq!(
        relay
            .handle(
                Input::Consumed {
                    connection: first.sender(),
                    session: first.wire_id(),
                    total: Counts::new(1, 21),
                },
                fixture.at
            )
            .refusal,
        None
    );
    assert_eq!(
        host.flush(&mut relay, fixture.at),
        CheckpointFlush::default()
    );
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow: flows[0],
                    credit: Counts::new(1, 21),
                },
                fixture.at
            )
            .refusal,
        None
    );
    host.flush(&mut relay, fixture.at);
    let receipt = receive(&mut relay, first, fixture.at);
    assert_eq!(
        relay.flush_checkpoints(fixture.at, |_, _| -> Result<(), ()> {
            panic!("unsubmitted payload")
        }),
        Err(CheckpointWriteError::State(Error::CheckpointBusy))
    );
    assert_eq!(
        relay
            .submit(&receipt, fixture.at, |_| Submission::Complete)
            .refusal,
        None
    );
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [first.sender()]);
    assert_eq!(host.bytes[&sibling.receiver()], sibling_bytes);

    let mut restored = host.restore(&fixture, stored);
    assert_eq!(
        restored
            .submit(&receipt, fixture.at, |_| panic!("already submitted"))
            .refusal,
        Some(Error::Flow(crate::flow::Refusal::SubmissionOrder))
    );
    assert_eq!(
        restored
            .handle(
                Input::Consumed {
                    connection: first.sender(),
                    session: first.wire_id(),
                    total: Counts::new(2, 42),
                },
                fixture.at
            )
            .refusal,
        None
    );
    assert_eq!(
        restored.sessions[&sibling.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
}

#[test]
fn admission_stage_changes_belong_to_the_receiver_attachment() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    let (sibling, _) = fixture.join(&mut relay, 3);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    activate(&mut relay, key, fixture.at);
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [key.receiver()]);
    let restored = host.restore(&fixture, stored);
    assert_eq!(
        restored.sessions[&key.wire_id()].session.phase(),
        crate::sessions::Phase::Active
    );
    assert_eq!(
        restored.sessions[&sibling.wire_id()].session.phase(),
        crate::sessions::Phase::AwaitingReceiver
    );
}

#[test]
fn partial_host_failure_retries_the_entire_batch_without_losing_updates() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, _) = fixture.join(&mut relay, 2);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let receipt = receive(&mut relay, key, fixture.at);
    let _ = relay.submit(&receipt, fixture.at, |_| Submission::Complete);
    let failed = relay.flush_checkpoints(fixture.at, |connection, bytes| {
        if connection == key.receiver() {
            return Err("host serialization failed");
        }
        host.bytes.insert(connection, bytes.to_vec());
        Ok(())
    });
    assert_eq!(
        failed,
        Err(CheckpointWriteError::Write {
            connection: key.receiver(),
            error: "host serialization failed",
        })
    );
    assert!(relay.observer().0.iter().any(|observation| matches!(observation,
        Observation::Relay { connection: Some(value), decision: RelayDecision::CheckpointWriteFailed, .. }
        if *value == key.receiver()
    )));
    assert_eq!(host.flush(&mut relay, fixture.at).connections, 2);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    let restored = host.restore(&fixture, stored);
    assert_eq!(
        restored.connections[&key.sender()].ledger.usage().submitted,
        Counts::new(1, 21)
    );
}

#[test]
fn closing_the_latest_receiver_does_not_lose_the_share_high_water_mark() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (first, _) = active(&mut relay, &fixture, 2);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let old_receiver =
        ConnectionCheckpoint::decode(&host.bytes[&first.receiver()], Fixture::limits()).unwrap();

    let (newest, flows) = fixture.join(&mut relay, 3);
    assert!(
        Checkpoint::from_connections(
            vec![
                relay.checkpoint_connection(first.sender()).unwrap(),
                old_receiver,
            ],
            Fixture::limits()
        )
        .is_err()
    );
    let _ = relay.handle(
        Input::Cancel {
            participant: newest.receiver(),
            session: newest.wire_id(),
        },
        fixture.at,
    );
    for flow in flows {
        let _ = relay.handle(Input::Discarded(flow), fixture.at);
    }
    let _ = relay.handle(Input::Closed(newest.receiver()), fixture.at);
    host.bytes.remove(&newest.receiver());
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [first.sender(), first.receiver()]);
    let mut restored = host.restore(&fixture, stored);
    let (next, _) = fixture.join(&mut restored, 4);
    assert_eq!(
        u64::from_be_bytes(*next.wire_id().as_bytes()),
        u64::from_be_bytes(*newest.wire_id().as_bytes()) + 1
    );
}

#[test]
fn retirement_attachments_restore_unsettled_occupancy_and_original_deadlines() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, flows) = active(&mut relay, &fixture, 2);
    active(&mut relay, &fixture, 3);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let _ = relay.handle(
        Input::Cancel {
            participant: key.receiver(),
            session: key.wire_id(),
        },
        fixture.at,
    );
    for flow in flows {
        let _ = relay.handle(Input::Discarded(flow), fixture.at);
    }
    let deadline = relay.deadline();
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    let mut restored = host.restore(&fixture, stored);
    assert_eq!(restored.deadline(), deadline);
    assert_eq!(
        restored.connections[&key.sender()]
            .ledger
            .snapshot(flows[0])
            .unwrap()
            .submitted,
        Counts::new(1, 21)
    );
    restored.enable_checkpoints(budget(), fixture.at).unwrap();
    host.flush(&mut restored, fixture.at);
    assert_eq!(
        restored
            .handle(
                Input::Settled {
                    connection: key.sender(),
                    session: key.wire_id(),
                    total: Counts::new(1, 21),
                },
                fixture.at
            )
            .refusal,
        None
    );
    host.flush(&mut restored, fixture.at);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    assert!(
        restored.connections[&key.sender()]
            .ledger
            .snapshot(flows[0])
            .is_none()
    );
}

#[test]
fn host_budget_checks_worst_case_metadata_before_tracking_is_enabled() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    assert!(CheckpointBudget::new(ATTACHMENT_BYTES, ATTACHMENT_BYTES).is_none());
    assert!(CheckpointBudget::new(ATTACHMENT_BYTES, usize::MAX).is_none());
    assert!(budget().check(PERSONAL_SESSION_LIMIT).is_ok());
    assert_eq!(
        budget().check(PERSONAL_SESSION_LIMIT * 2),
        Err(Error::CheckpointCapacity)
    );
    assert_eq!(ConnectionCheckpoint::maximum_bytes(usize::MAX), None);
    let maximum = ConnectionCheckpoint::maximum_bytes(Fixture::limits().sessions).unwrap();
    let exact = CheckpointBudget::new(maximum + HOST_ENVELOPE_BYTES, HOST_ENVELOPE_BYTES).unwrap();
    assert!(exact.check(Fixture::limits().sessions).is_ok());
    let short = CheckpointBudget::new(maximum - 1, 0).unwrap();
    assert_eq!(
        relay.enable_checkpoints(short, fixture.at),
        Err(Error::CheckpointCapacity)
    );
    assert!(matches!(
        relay.checkpointing,
        checkpoint_tracking::Checkpointing::Disabled
    ));
    relay.enable_checkpoints(exact, fixture.at).unwrap();
    fixture.register(&mut relay);
    for receiver in 2..=5 {
        fixture.join(&mut relay, receiver);
    }
    let sender = relay.checkpoint_connection(connection(1)).unwrap();
    let receiver = relay.checkpoint_connection(connection(2)).unwrap();
    assert_eq!(sender.ledger.flows().count(), 4);
    assert_eq!(receiver.ledger.flows().count(), 1);
    assert_eq!(sender.sessions.len(), 0);
    assert_eq!(receiver.sessions.len(), 1);
    for snapshot in relay.checkpoint().unwrap().connections {
        assert!(snapshot.encode().len() <= maximum);
    }
}

#[test]
fn independent_checkpoint_codecs_reject_mixed_or_misassigned_state() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    fixture.join(&mut relay, 2);
    let sender = relay.checkpoint_connection(connection(1)).unwrap();
    let receiver = relay.checkpoint_connection(connection(2)).unwrap();
    let bytes = receiver.encode();
    for bytes in [&bytes[..0], &bytes[..1], &bytes[..bytes.len() - 1]] {
        assert!(ConnectionCheckpoint::decode(bytes, Fixture::limits()).is_err());
    }
    let oversized =
        vec![0; ConnectionCheckpoint::maximum_bytes(Fixture::limits().sessions).unwrap() + 1];
    assert!(ConnectionCheckpoint::decode(&oversized, Fixture::limits()).is_err());
    assert!(Checkpoint::from_connections(Vec::new(), Fixture::limits()).is_err());
    assert!(
        Checkpoint::from_connections(vec![sender.clone(), sender.clone()], Fixture::limits())
            .is_err()
    );
    let mut wrong_connection = sender.clone();
    wrong_connection.connection = connection(5);
    assert!(ConnectionCheckpoint::decode(&wrong_connection.encode(), Fixture::limits()).is_err());
    let mut wrong_admission = sender.clone();
    wrong_admission.sessions = receiver.sessions.clone();
    assert!(ConnectionCheckpoint::decode(&wrong_admission.encode(), Fixture::limits()).is_err());
    let mut wrong_identity = receiver;
    wrong_identity.identity = fixture.identity(9);
    assert!(Checkpoint::from_connections(vec![sender, wrong_identity], Fixture::limits()).is_err());
}

#[test]
fn handshake_expiration_and_storage_barriers_are_included_in_incremental_capture() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let publish = fixture.begin_registration(&mut relay);
    assert_eq!(
        relay.flush_checkpoints(fixture.at, |_, _| -> Result<(), ()> {
            panic!("unfinished storage")
        }),
        Err(CheckpointWriteError::State(Error::CheckpointBusy))
    );
    let durable = work(&complete(&mut relay, &publish, fixture.at));
    let _ = complete(&mut relay, &durable, fixture.at);
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let _ = relay.handle(Input::Connected(connection(2)), fixture.at);
    host.flush(&mut relay, fixture.at);
    assert_eq!(host.written, [connection(2)]);
    let deadline = relay.deadline().unwrap();
    let _ = relay.handle(Input::Tick, deadline);
    host.flush(&mut relay, deadline);
    assert_eq!(host.written, [connection(2)]);
    assert!(matches!(
        host.checkpoint().connections[1].role,
        checkpoint::ConnectionRole::Closing
    ));
    let _ = relay.handle(Input::Closed(connection(2)), deadline);
    host.bytes.remove(&connection(2));
    assert_eq!(host.flush(&mut relay, deadline), CheckpointFlush::default());
}
