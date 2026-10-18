use super::{
    failure,
    fixture::{self, Fixture},
    persistence::{self, Image, Mode},
};
use js_sys::Date;
use serde_json::{Value, json};
use std::collections::VecDeque;
use windshare_relay_core::{
    flow::Counts,
    lifecycle::{ConnectionRef, Record},
    observation::{Observation, Observer},
    sessions::OpaqueHeader,
    share::{
        CheckpointTarget, Control, Input, Output, ShareRelay, Storage, Submission, Turn, WorkResult,
    },
};
use worker::{Result, State};

pub struct Trace {
    pub object: String,
    pub boot: String,
}
impl Observer for Trace {
    fn observe(&mut self, observation: Observation) {
        // Metadata only. Payloads stay in the event and never reach SQL or logs.
        worker::console_log!(
            "{}",
            json!({"event":"checkpoint_kernel", "object":self.object,
            "boot":self.boot, "at_ms":Date::now(), "decision":format!("{observation:?}")})
        );
    }
}

pub struct Runtime {
    pub relay: ShareRelay<Trace>,
    pub image: Image,
    pub fixture: Fixture,
    pub last_refusal: Option<String>,
}

impl Runtime {
    pub fn apply(&mut self, state: &State, input: Input<'_>, payload: Option<&[u8]>) -> Result<()> {
        let turn = self.relay.handle(input, self.fixture.now());
        self.outputs(state, turn, payload)
    }

    pub fn outputs(&mut self, state: &State, turn: Turn, payload: Option<&[u8]>) -> Result<()> {
        let mut queue = VecDeque::from([turn]);
        while let Some(turn) = queue.pop_front() {
            if let Some(reason) = turn.refusal {
                self.last_refusal = Some(format!("{reason:?}"));
            }
            for output in turn.outputs {
                let follow = match output {
                    Output::ChallengeNeeded(request) => Some(self.relay.handle(
                        Input::Challenge {
                            request,
                            seed: self.fixture.seed(),
                        },
                        self.fixture.now(),
                    )),
                    Output::Work(work) => {
                        let current = self.image.record()?;
                        match work.operation {
                            Storage::Publish { record, .. } => {
                                if current.is_some() && current != Some(*record) {
                                    return Err(failure("publication conflict"));
                                }
                                self.image.lifecycle = Some(record.encode().as_bytes().to_vec());
                            }
                            Storage::CompareExchange { expected, next } => {
                                if current != expected.as_deref().copied()
                                    && current != next.as_deref().copied()
                                {
                                    return Err(failure("lifecycle compare-exchange conflict"));
                                }
                                self.image.lifecycle =
                                    next.map(|record| record.encode().as_bytes().to_vec());
                            }
                            Storage::RemovePublication(id) => {
                                if matches!(current, Some(Record::Unresolved(record)) if record.id() == id)
                                {
                                    self.image.lifecycle = None;
                                }
                            }
                        }
                        persistence::save(state, &mut self.image)?;
                        Some(self.relay.handle(
                            Input::Completed {
                                id: work.id,
                                result: WorkResult::Complete,
                            },
                            self.fixture.now(),
                        ))
                    }
                    Output::Received(receipt) => {
                        let bytes = payload.ok_or_else(|| failure("missing event payload"))?;
                        Some(self.relay.submit(&receipt, self.fixture.now(), |flow| {
                            match persistence::socket(state, flow.destination()) {
                                Some(ws) if ws.send_with_bytes(bytes).is_ok() => {
                                    Submission::Complete
                                }
                                _ => Submission::Uncertain,
                            }
                        }))
                    }
                    Output::Discard(flow) => {
                        // No application queue survives an event. Submission happened synchronously.
                        Some(
                            self.relay
                                .handle(Input::Discarded(flow), self.fixture.now()),
                        )
                    }
                    Output::Close(connection) => {
                        if let Some(ws) = persistence::socket(state, connection) {
                            ws.close(Some(1000), Some("kernel closure requested"))?;
                        }
                        None
                    }
                    Output::Send {
                        connection,
                        control,
                    } => {
                        if let Some(ws) = persistence::socket(state, connection) {
                            match control {
                                Control::ProbeAck(ack) => ws.send_with_bytes(ack.encode())?,
                                Control::Settlement(notice) => ws.send_with_bytes(notice.encode())?,
                                Control::Descriptor { session, .. } => ws.send_with_str(json!({
                                    "event":"joined", "session":u64::from_be_bytes(*session.wire_id().as_bytes()),
                                }).to_string())?,
                                Control::Grant(grant) => ws.send_with_str(json!({
                                    "event":"grant", "session":u64::from_be_bytes(*grant.flow.session().wire_id().as_bytes()),
                                }).to_string())?,
                                Control::Registered(_) => ws.send_with_str("{\"event\":\"registered\"}")?,
                                Control::Error(error) => ws.send_with_str(json!({"event":"kernel_error",
                                    "detail":format!("{error:?}")}).to_string())?,
                                Control::Challenge(_) | Control::Stopped(_) => {}
                            }
                        }
                        None
                    }
                    other => return Err(failure(format!("unhandled host obligation: {other:?}"))),
                };
                if let Some(follow) = follow {
                    queue.push_back(follow);
                }
            }
        }
        Ok(())
    }

    pub fn begin(&mut self, state: &State) -> Result<()> {
        self.last_refusal = None;
        if self.image.mode == Mode::SqliteMirror {
            // If the end-of-event snapshot fails, recovery must not trust its older counters.
            self.image.pending = true;
            persistence::save(state, &mut self.image)?;
        }
        Ok(())
    }

    pub fn flush(&mut self, state: &State) -> Result<()> {
        let mut next = self.image.clone();
        let mut durable_changed = next.mode == Mode::SqliteMirror;
        self.relay
            .flush_checkpoints(self.fixture.now(), |target, bytes| -> Result<()> {
                match target {
                    CheckpointTarget::Generation(connection) => {
                        let socket = persistence::socket(state, connection);
                        if let Some(ws) = &socket {
                            persistence::write_attachment(ws, bytes)?;
                        }
                        if next.mode == Mode::SqliteMirror {
                            next.records
                                .insert(persistence::tag(connection), bytes.to_vec());
                            durable_changed = true;
                        }
                    }
                    CheckpointTarget::Manifest => {
                        next.manifest = bytes.to_vec();
                        durable_changed = true;
                    }
                }
                Ok(())
            })
            .map_err(failure)?;
        let manifest = self.relay.checkpoint_manifest().map_err(failure)?;
        next.records
            .retain(|key, _| manifest.generations().any(|c| persistence::tag(c) == *key));
        next.pending = false;
        if durable_changed {
            persistence::save(state, &mut next)?;
        }
        self.image = next;
        Ok(())
    }

    pub fn register(&mut self, state: &State, connection: ConnectionRef) -> Result<()> {
        self.apply(state, Input::Connected(connection), None)?;
        for field in ["registerInitB64", "registerProofB64", "descriptorUploadB64"] {
            let frame = self.fixture.frame(field);
            self.apply(
                state,
                Input::Control {
                    connection,
                    frame: &frame,
                },
                None,
            )?;
        }
        Ok(())
    }

    pub fn join(&mut self, state: &State, connection: ConnectionRef) -> Result<()> {
        self.apply(state, Input::Connected(connection), None)?;
        let frame = self.fixture.frame("joinB64");
        let turn = self.relay.handle(
            Input::Control {
                connection,
                frame: &frame,
            },
            self.fixture.now(),
        );
        let flows: Vec<_> = turn
            .outputs
            .iter()
            .filter_map(|output| match output {
                Output::Send {
                    control: Control::Grant(grant),
                    ..
                } => Some(grant.flow),
                _ => None,
            })
            .collect();
        self.outputs(state, turn, None)?;
        // Reserve the bounded burst at admission. Forwarding inspects only WS2O's
        // routing header; application payload contents never control grant policy.
        for flow in flows {
            self.apply(
                state,
                Input::Grant {
                    flow,
                    credit: Counts::new(
                        fixture::WINDOW_FRAMES - 1,
                        (fixture::WINDOW_FRAMES - 1) * u64::from(OpaqueHeader::MAX_ENCODED_BYTES),
                    ),
                },
                None,
            )?;
        }
        Ok(())
    }

    pub fn forward(&mut self, state: &State, source: ConnectionRef, bytes: &[u8]) -> Result<()> {
        let turn = self.relay.handle(
            Input::Frame {
                source,
                header: OpaqueHeader::decode(bytes).map_err(failure)?,
            },
            self.fixture.now(),
        );
        self.outputs(state, turn, Some(bytes))
    }

    pub fn status(&self, state: &State) -> Result<Value> {
        let manifest = self.relay.checkpoint_manifest().map_err(failure)?;
        let usage: Vec<_> = manifest.generations().map(|connection| {
            let used = self.relay.delivery_usage(connection);
            let reservation = self.relay.unavailable_reservation(connection);
            json!({"connection":persistence::tag(connection),
                "communicating":persistence::socket(state, connection).is_some(),
                "accounting":if used.is_some() { "exact" } else { "unavailable" },
                "reserved_frames":reservation.map(|bound| bound.frames),
                "reserved_bytes":reservation.map(|bound| bound.encoded_bytes),
                "granted_frames":used.map(|used| used.granted.frames),
                "submitted_frames":used.map(|used| used.submitted.frames), "submitted_bytes":used.map(|used| used.submitted.encoded_bytes)})
        }).collect();
        let admission = self.relay.connection_usage();
        Ok(
            json!({"ready":true, "mode":self.image.mode, "writes":self.image.writes,
            "generations":usage, "last_refusal":self.last_refusal,
            "admitted":admission.business, "controls":admission.controls, "retired_generations":admission.retired_generations,
            "residual_frames":admission.residual.frames, "residual_bytes":admission.residual.encoded_bytes,
            "kernel_deadline_ms":self.relay.deadline().map(|at| at.0),
            "physical_release_proven":false, "clock":"frozen canonical fixture"}),
        )
    }
}
