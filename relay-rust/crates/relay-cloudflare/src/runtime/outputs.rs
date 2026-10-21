use super::{Host, Runtime};
use crate::config;
use anyhow::{Result, anyhow, bail, ensure};
use std::collections::VecDeque;
use windshare_relay_core::{
    flow::Counts,
    observation::Observer,
    sessions::encode_descriptor,
    share::{Control, Input, Output, RelayStep, WorkResult},
};

impl<O: Observer> Runtime<O> {
    pub(super) fn outputs(&mut self, host: &mut impl Host, step: RelayStep) -> Result<()> {
        let mut outputs: VecDeque<_> = step.outputs.into();
        loop {
            while let Some(output) = outputs.pop_front() {
                if let Some(follow) = self.output(host, output)? {
                    // Cleanup receipts produce ordered settlement notices. Finish
                    // those effects before a later Close or STOP acknowledgement.
                    for output in follow.outputs.into_iter().rev() {
                        outputs.push_front(output);
                    }
                }
            }
            // The kernel owns both routing and occupancy, including after hibernation.
            // No host credit counter or independently restored scheduler can refund debt.
            let offer = self.relay.delivery_flows().find_map(|flow| {
                self.relay
                    .delivery_window(flow, config::delivery_window())
                    .filter(|credit| *credit != Counts::ZERO)
                    .map(|credit| (flow, credit))
            });
            let Some((flow, credit)) = offer else {
                return Ok(());
            };
            let step = self.relay.handle(Input::Grant { flow, credit }, host.now());
            outputs.extend(step.outputs);
            if step.refusal.is_some() {
                // Time can advance between the advisory offer and its reservation.
                // Retire only this session; a shared sender still serves its siblings.
                outputs.extend(
                    self.relay
                        .handle(
                            Input::Cancel {
                                participant: flow.permit().source(),
                                session: flow.session().wire_id(),
                            },
                            host.now(),
                        )
                        .outputs,
                );
            }
        }
    }

    fn output(&mut self, host: &mut impl Host, output: Output) -> Result<Option<RelayStep>> {
        Ok(match output {
            Output::ChallengeNeeded(request) => Some(self.relay.handle(
                Input::Challenge {
                    request,
                    seed: host.challenge()?,
                },
                host.now(),
            )),
            Output::Work(work) => {
                // Only the confirmed atomic row may supply a JOIN descriptor.
                // SQLite's output gate holds subsequent sends until durability.
                self.authority = host.execute(&work.operation)?;
                Some(self.relay.handle(
                    Input::Completed {
                        id: work.id,
                        result: WorkResult::Complete,
                    },
                    host.now(),
                ))
            }
            Output::Send {
                connection,
                control,
            } => {
                if let Control::Grant(grant) = &control
                    && self
                        .relay
                        .delivery_window(grant.flow, Counts::ZERO)
                        .is_none()
                {
                    return Ok(None);
                }
                if host.send(connection, &self.encode(control)?).is_err() {
                    host.close(connection)?;
                    Some(
                        self.relay
                            .handle(Input::Disconnected(connection), host.now()),
                    )
                } else {
                    None
                }
            }
            Output::Close(connection) => {
                host.close(connection)?;
                // Communication loss never proves physical buffer release.
                Some(
                    self.relay
                        .handle(Input::Disconnected(connection), host.now()),
                )
            }
            Output::Discard(flow) => {
                // Payloads live only in the current message callback. Forwarding
                // drops that payload before executing a step that can discard it.
                Some(self.relay.handle(Input::Discarded(flow), host.now()))
            }
            Output::Retry(_) | Output::Reload => bail!("authoritative recovery required"),
            // This host already revisits its bounded ledgers after every event,
            // including reconstruction from hibernation.
            Output::DeliveryReady(_) => None,
            Output::Received(_) => bail!("received receipt without its ingress payload"),
        })
    }

    fn encode(&self, control: Control) -> Result<Vec<u8>> {
        Ok(match control {
            Control::Challenge(bytes) => bytes,
            Control::Error(frame) => frame.encode().to_vec(),
            Control::Registered(frame) => frame.encode().to_vec(),
            Control::Stopped(frame) => frame.encode().to_vec(),
            Control::ProbeAck(frame) => frame.encode().to_vec(),
            Control::Grant(grant) => grant.encode()?.to_vec(),
            Control::Settlement(notice) => notice.encode().to_vec(),
            Control::Descriptor { session, record } => {
                let authority = self
                    .authority
                    .as_ref()
                    .ok_or_else(|| anyhow!("descriptor authority missing"))?;
                ensure!(
                    authority.record() == *record,
                    "descriptor authority changed"
                );
                let descriptor = authority
                    .descriptor()
                    .ok_or_else(|| anyhow!("committed descriptor missing"))?;
                encode_descriptor(session.wire_id(), descriptor)?
            }
        })
    }
}
