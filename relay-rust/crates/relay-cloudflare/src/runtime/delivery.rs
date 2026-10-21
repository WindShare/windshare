use super::{Host, Runtime};
use anyhow::Result;
use windshare_relay_core::{
    lifecycle::ConnectionRef,
    observation::Observer,
    sessions::OpaqueHeader,
    share::{Input, Output, Submission},
};

impl<O: Observer> Runtime<O> {
    pub(crate) fn forward(
        &mut self,
        host: &mut impl Host,
        source: ConnectionRef,
        bytes: Vec<u8>,
    ) -> Result<()> {
        let Ok(header) = OpaqueHeader::decode(&bytes) else {
            drop(bytes);
            self.disconnect(host, source)?;
            return self.flush(host);
        };
        let mut step = self
            .relay
            .handle(Input::Frame { source, header }, host.now());
        let refused = step.refusal.is_some();
        let receipt = step
            .outputs
            .iter()
            .position(|output| matches!(output, Output::Received(_)))
            .map(|index| step.outputs.remove(index));
        if let Some(Output::Received(receipt)) = receipt {
            // No await separates receive, permit recheck, accounting and send.
            // Failed sends retain their charge; the kernel retires the destination.
            let submitted = self.relay.submit(&receipt, host.now(), |flow| {
                if host.send(flow.destination(), &bytes).is_ok() {
                    Submission::Complete
                } else {
                    Submission::Uncertain
                }
            });
            step.outputs.extend(submitted.outputs);
        }
        // Destroy the event-owned buffer before acknowledging any queue discard.
        drop(bytes);
        self.outputs(host, step)?;
        if refused {
            self.disconnect(host, source)?;
        }
        self.flush(host)
    }
}
