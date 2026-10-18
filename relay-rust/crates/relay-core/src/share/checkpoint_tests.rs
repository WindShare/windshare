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
    manifest: Option<CheckpointManifest>,
    bytes: BTreeMap<ConnectionRef, Vec<u8>>,
    written: Vec<ConnectionRef>,
}

impl Attachments {
    fn flush(&mut self, relay: &mut Relay, now: UnixMillis) -> CheckpointFlush {
        self.written.clear();
        let result = relay
            .flush_checkpoints(now, |target, bytes| {
                match target {
                    CheckpointTarget::Generation(connection) => {
                        assert!(bytes.len() + HOST_ENVELOPE_BYTES <= ATTACHMENT_BYTES);
                        self.bytes.insert(connection, bytes.to_vec());
                        self.written.push(connection);
                    }
                    CheckpointTarget::Manifest => {
                        let manifest =
                            CheckpointManifest::decode(bytes, Fixture::limits()).unwrap();
                        self.bytes.retain(|connection, _| {
                            manifest.generations().any(|value| value == *connection)
                        });
                        self.manifest = Some(manifest);
                    }
                }
                Ok::<_, ()>(())
            })
            .unwrap();
        let checkpoint = self.checkpoint();
        assert_eq!(checkpoint.encode(), relay.checkpoint().unwrap().encode());
        result
    }

    fn checkpoint(&self) -> Checkpoint {
        Checkpoint::from_connections(
            self.manifest.as_ref().unwrap(),
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
                authority: Authority::Known(Some(stored)),
                surviving: &self.bytes.keys().copied().collect::<Vec<_>>(),
                responsibility: Responsibility::Checkpoint(&checkpoint),
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
fn same_instant_data_and_feedback_only_update_accounting_and_replay_writes_nothing() {
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
fn later_inbound_data_and_probes_persist_source_liveness_without_rewriting_siblings() {
    use crate::heartbeat::{
        CONNECTION_PROBE_INTERVAL, CONNECTION_SILENCE_TIMEOUT, ConnectionProbe,
    };

    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let stored = fixture.register(&mut relay);
    let (key, _) = active(&mut relay, &fixture, 2);
    let (sibling, _) = active(&mut relay, &fixture, 3);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let sibling_bytes = host.bytes[&sibling.receiver()].clone();
    let at = fixture.at.saturating_add(CONNECTION_PROBE_INTERVAL);
    let receipt = receive_from(&mut relay, key, key.sender(), at);
    assert_eq!(
        relay.submit(&receipt, at, |_| Submission::Complete).refusal,
        None
    );
    let flushed = host.flush(&mut relay, at);
    assert_eq!(host.written, [key.sender(), key.receiver()]);
    assert!(!flushed.manifest);
    assert_eq!(host.bytes[&sibling.receiver()], sibling_bytes);

    let frame = ConnectionProbe::new(NonZeroU64::new(1).unwrap()).encode();
    let turn = relay.handle(
        Input::Control {
            connection: key.sender(),
            frame: &frame,
        },
        at,
    );
    assert_eq!(turn.refusal, None);
    assert_eq!(host.flush(&mut relay, at).connections, 1);
    assert_eq!(host.written, [key.sender()]);
    let checkpoint = host.checkpoint();
    let (mut restored, _) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender(), key.receiver(), sibling.receiver()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        at,
    )
    .unwrap();
    let Role::Established(live) = restored.generations.connection(&key.sender()).unwrap().role
    else {
        panic!("lost sender");
    };
    assert_eq!(live.last_probe_nonce(), 1);
    assert_eq!(live.last_inbound(), at);
    assert_eq!(
        live.deadline(),
        at.saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert_eq!(
        restored
            .handle(
                Input::Control {
                    connection: key.sender(),
                    frame: &frame
                },
                at
            )
            .refusal,
        Some(Error::ProbeNonce)
    );
}

#[test]
fn accepted_repeated_feedback_updates_liveness_without_releasing_credit_twice() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, _) = active(&mut relay, &fixture, 2);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let frame = crate::flow::DeliveryConsumed::new(key.wire_id(), Counts::new(1, 21))
        .unwrap()
        .encode();
    assert_eq!(
        relay
            .handle(
                Input::Control {
                    connection: key.sender(),
                    frame: &frame
                },
                fixture.at
            )
            .refusal,
        None
    );
    host.flush(&mut relay, fixture.at);
    let usage = relay.delivery_usage(key.sender());
    let at = fixture
        .at
        .saturating_add(crate::heartbeat::CONNECTION_PROBE_INTERVAL);
    assert_eq!(
        relay
            .handle(
                Input::Control {
                    connection: key.sender(),
                    frame: &frame
                },
                at
            )
            .refusal,
        None
    );
    assert_eq!(relay.delivery_usage(key.sender()), usage);
    assert!(!host.flush(&mut relay, at).manifest);
    assert_eq!(host.written, [key.sender()]);
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
    let failed = relay.flush_checkpoints(fixture.at, |target, bytes| {
        let CheckpointTarget::Generation(connection) = target else {
            panic!("ordinary delivery does not change the manifest")
        };
        if connection == key.receiver() {
            return Err("host serialization failed");
        }
        host.bytes.insert(connection, bytes.to_vec());
        Ok(())
    });
    assert_eq!(
        failed,
        Err(CheckpointWriteError::Write {
            target: CheckpointTarget::Generation(key.receiver()),
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
        ledger_of(&restored, key.sender()).usage().submitted,
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
    let partial = Checkpoint::from_connections(
        &relay.checkpoint_manifest().unwrap(),
        vec![
            relay.checkpoint_connection(first.sender()).unwrap(),
            old_receiver,
        ],
        Fixture::limits(),
    )
    .unwrap();
    assert_eq!(partial.connections.len(), 2);
    assert_eq!(partial.manifest.last_session, relay.last_session);
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
    assert_eq!(host.written, [first.sender()]);
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
        ledger_of(&restored, key.sender())
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
        ledger_of(&restored, key.sender())
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
    let maximum =
        ConnectionCheckpoint::maximum_bytes(Fixture::limits().retained_flows_per_connection)
            .unwrap();
    let exact = CheckpointBudget::new(maximum + HOST_ENVELOPE_BYTES, HOST_ENVELOPE_BYTES).unwrap();
    assert!(
        exact
            .check(Fixture::limits().retained_flows_per_connection)
            .is_ok()
    );
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
    assert_eq!(sender.ledger().unwrap().flows().count(), 4);
    assert_eq!(receiver.ledger().unwrap().flows().count(), 1);
    assert_eq!(sender.sessions().len(), 0);
    assert_eq!(receiver.sessions().len(), 1);
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
    let manifest = relay.checkpoint_manifest().unwrap();
    for bytes in [&bytes[..0], &bytes[..1], &bytes[..bytes.len() - 1]] {
        assert!(ConnectionCheckpoint::decode(bytes, Fixture::limits()).is_err());
    }
    let oversized =
        vec![
            0;
            ConnectionCheckpoint::maximum_bytes(Fixture::limits().retained_flows_per_connection)
                .unwrap()
                + 1
        ];
    assert!(ConnectionCheckpoint::decode(&oversized, Fixture::limits()).is_err());
    assert!(
        Checkpoint::from_connections(&manifest, Vec::new(), Fixture::limits())
            .unwrap()
            .connections
            .is_empty()
    );
    assert!(
        Checkpoint::from_connections(
            &manifest,
            vec![sender.clone(), sender.clone()],
            Fixture::limits()
        )
        .unwrap()
        .connections
        .is_empty()
    );
    let mut wrong_connection = sender.clone();
    wrong_connection.connection = connection(5);
    assert!(ConnectionCheckpoint::decode(&wrong_connection.encode(), Fixture::limits()).is_err());
    let mut wrong_admission = sender.clone();
    let checkpoint::ConnectionResources::Business { sessions, .. } = &mut wrong_admission.resources
    else {
        panic!("business checkpoint")
    };
    *sessions = receiver.sessions().to_vec();
    assert!(ConnectionCheckpoint::decode(&wrong_admission.encode(), Fixture::limits()).is_err());
    let mut wrong_identity = receiver;
    wrong_identity.identity = fixture.identity(9);
    let partial =
        Checkpoint::from_connections(&manifest, vec![sender, wrong_identity], Fixture::limits())
            .unwrap();
    assert_eq!(partial.connections.len(), 1);
    assert_eq!(partial.connections[0].connection, connection(1));
}

#[test]
fn manifest_detects_a_missing_generation_even_when_its_socket_is_gone() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    fixture.join(&mut relay, 2);
    let manifest = relay.checkpoint_manifest().unwrap();
    let sender = relay.checkpoint_connection(connection(1)).unwrap();
    let partial = Checkpoint::from_connections(&manifest, vec![sender], Fixture::limits()).unwrap();
    assert_eq!(partial.connections.len(), 1);
    assert_eq!(partial.manifest.generations().count(), 2);
    let bytes = manifest.encode();
    assert_eq!(
        CheckpointManifest::decode(&bytes, Fixture::limits()).unwrap(),
        manifest
    );
    for bytes in [&bytes[..0], &bytes[..1], &bytes[..bytes.len() - 1]] {
        assert!(CheckpointManifest::decode(bytes, Fixture::limits()).is_err());
    }
    let mut reduced = Fixture::limits();
    reduced.business_connections = 1;
    assert!(CheckpointManifest::decode(&bytes, reduced).is_err());
    let mut duplicate = manifest;
    duplicate.reservations[1] = duplicate.reservations[0];
    assert!(CheckpointManifest::decode(&duplicate.encode(), Fixture::limits()).is_err());
    let empty = fixture.relay().checkpoint_manifest().unwrap();
    let empty = CheckpointManifest::decode(&empty.encode(), Fixture::limits()).unwrap();
    assert!(Checkpoint::from_connections(&empty, Vec::new(), Fixture::limits()).is_ok());
}

#[test]
fn manifest_failure_retries_generation_records_and_only_commits_removal_after_release() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    let _ = relay.handle(Input::Connected(connection(1)), fixture.at);
    // A business reservation survives communication loss until physical release.
    let _ = fixture.control(&mut relay, connection(1), "resumeInitB64", fixture.at);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    let result = relay.flush_checkpoints(fixture.at, |target, bytes| {
        match target {
            CheckpointTarget::Generation(connection) => {
                host.bytes.insert(connection, bytes.to_vec());
            }
            CheckpointTarget::Manifest => return Err("manifest unavailable"),
        }
        Ok(())
    });
    assert_eq!(
        result,
        Err(CheckpointWriteError::Write {
            target: CheckpointTarget::Manifest,
            error: "manifest unavailable"
        })
    );
    let flushed = host.flush(&mut relay, fixture.at);
    assert_eq!(flushed.connections, 1);
    assert!(flushed.manifest);
    let _ = relay.handle(Input::Disconnected(connection(1)), fixture.at);
    assert!(!host.bytes.is_empty());
    let flushed = host.flush(&mut relay, fixture.at);
    assert_eq!(flushed.connections, 1);
    assert!(flushed.manifest);
    let _ = relay.handle(Input::Closed(connection(1)), fixture.at);
    let flushed = host.flush(&mut relay, fixture.at);
    assert_eq!(flushed.connections, 0);
    assert!(flushed.manifest);
    assert!(host.bytes.is_empty());
}

#[test]
fn detached_generations_keep_receiving_checkpoint_updates_without_socket_attachments() {
    let fixture = Fixture::new();
    let mut relay = fixture.relay();
    fixture.register(&mut relay);
    let (key, flows) = active(&mut relay, &fixture, 2);
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    let mut host = Attachments::default();
    host.flush(&mut relay, fixture.at);
    let detached = relay.handle(Input::Disconnected(key.sender()), fixture.at);
    let _ = complete(&mut relay, &work(&detached), fixture.at);
    for flow in flows {
        let _ = relay.handle(Input::Discarded(flow), fixture.at);
    }
    host.flush(&mut relay, fixture.at);
    let stored = next_record(&work(&detached));
    let checkpoint = host.checkpoint();
    assert!(matches!(
        checkpoint.connections[0].role,
        checkpoint::ConnectionRole::Detached(_)
    ));
    let (detached, turn) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore {
            authority: Authority::Known(Some(stored)),
            surviving: &[key.sender()],
            responsibility: Responsibility::Checkpoint(&checkpoint),
        },
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    assert!(detached.generations.connection(&key.sender()).is_none());
    assert!(
        turn.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(value) if *value == key.sender()))
    );
    let (mut restored, turn) = Relay::restore(
        fixture.identity(2),
        Fixture::limits(),
        &Restore::restarted(Some(stored), Responsibility::Checkpoint(&checkpoint)),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    for output in turn.outputs {
        if let Output::Discard(flow) = output {
            let _ = restored.handle(Input::Discarded(flow), fixture.at);
        }
    }
    restored.enable_checkpoints(budget(), fixture.at).unwrap();
    host.flush(&mut restored, fixture.at);
    assert_eq!(
        restored
            .handle(
                Input::Settled {
                    connection: key.sender(),
                    session: key.wire_id(),
                    total: Counts::new(1, 21)
                },
                fixture.at
            )
            .refusal,
        None
    );
    assert_eq!(
        restored.delivery_usage(key.sender()).unwrap().submitted,
        Counts::ZERO
    );
    let _ = restored.handle(Input::Closed(key.sender()), fixture.at);
    host.flush(&mut restored, fixture.at);
    assert!(
        !host
            .checkpoint()
            .manifest()
            .generations()
            .any(|connection| connection == key.sender())
    );
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
    let _ = complete(&mut relay, &publish, fixture.at);
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
    let flushed = host.flush(&mut relay, deadline);
    assert_eq!(flushed.connections, 0);
    assert!(flushed.manifest);
}

#[test]
fn retired_feedback_updates_its_record_and_manifest_as_one_accounting_change() {
    const ONE: Counts = Counts::new(1, 21);
    const TWO: Counts = Counts::new(2, 42);
    let fixture = Fixture::new();
    let limits = Limits {
        retirement: RetirementPolicy::Bounded {
            grace: Duration::ZERO,
            generations: 1,
            residual: TWO,
        },
        ..Fixture::limits()
    };
    let (mut relay, _) = Relay::restore(
        fixture.identity(1),
        limits,
        &Restore::restarted(None, Responsibility::Released { previous: None }),
        Trace::default(),
        fixture.at,
    )
    .unwrap();
    fixture.register(&mut relay);
    let (key, flows) = active(&mut relay, &fixture, 2);
    assert_eq!(
        relay
            .handle(
                Input::Grant {
                    flow: flows[1],
                    credit: ONE
                },
                fixture.at
            )
            .refusal,
        None
    );
    for _ in 0..TWO.frames {
        let receipt = receive_from(&mut relay, key, key.sender(), fixture.at);
        assert_eq!(
            relay
                .submit(&receipt, fixture.at, |_| Submission::Complete)
                .refusal,
            None
        );
    }
    let _ = relay.handle(Input::Disconnected(key.receiver()), fixture.at);
    for flow in flows {
        assert_eq!(
            relay.handle(Input::Discarded(flow), fixture.at).refusal,
            None
        );
    }
    assert_eq!(relay.connection_usage().residual, TWO);
    let feedback = || Input::Consumed {
        connection: key.receiver(),
        session: key.wire_id(),
        total: ONE,
    };
    relay.enable_checkpoints(budget(), fixture.at).unwrap();
    relay
        .flush_checkpoints(fixture.at, |_, _| Ok::<_, ()>(()))
        .unwrap();

    assert_eq!(relay.handle(feedback(), fixture.at).refusal, None);
    assert_eq!(relay.connection_usage().residual, ONE);
    let expected = relay.checkpoint_manifest().unwrap();
    let mut written = Vec::new();
    relay
        .flush_checkpoints(fixture.at, |target, bytes| {
            written.push(target);
            if target == CheckpointTarget::Manifest {
                assert_eq!(CheckpointManifest::decode(bytes, limits).unwrap(), expected);
            }
            Ok::<_, ()>(())
        })
        .unwrap();
    assert_eq!(
        written,
        [
            CheckpointTarget::Generation(key.receiver()),
            CheckpointTarget::Manifest
        ]
    );

    // Replayed feedback changes neither exact occupancy nor the durable risk bound.
    assert_eq!(relay.handle(feedback(), fixture.at).refusal, None);
    let flushed = relay
        .flush_checkpoints(fixture.at, |_, _| -> Result<(), ()> {
            panic!("unchanged generation must not be persisted");
        })
        .unwrap();
    assert_eq!(flushed.connections, 0);
    assert!(!flushed.manifest);
}
