use super::{Runtime, WriteOrigin, budget::WriteAttempt};
use std::{
    future::Future,
    io,
    pin::Pin,
    task::{Context, Poll, ready},
};
use tokio::{
    io::{AsyncRead, AsyncWrite, ReadBuf},
    task::JoinHandle,
};

pub struct Metered<S> {
    stream: S,
    origin: WriteOrigin,
    node: Runtime,
    epoch: u64,
    refill: Option<JoinHandle<anyhow::Result<()>>>,
}

impl<S> Metered<S> {
    pub(crate) const fn new(stream: S, node: Runtime, epoch: u64, origin: WriteOrigin) -> Self {
        Self {
            stream,
            origin,
            node,
            epoch,
            refill: None,
        }
    }
}

impl<S: AsyncRead + Unpin> AsyncRead for Metered<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_read(context, buffer)
    }
}

impl<S: AsyncWrite + Unpin> AsyncWrite for Metered<S> {
    fn poll_write(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<io::Result<usize>> {
        let this = self.get_mut();
        loop {
            if let Some(refill) = this.refill.as_mut() {
                let result = ready!(Pin::new(refill).poll(context));
                this.refill = None;
                result
                    .map_err(io::Error::other)?
                    .map_err(io::Error::other)?;
            }
            let timestamp = super::now();
            let attempt = this.node.write_egress(
                this.epoch,
                &this.origin,
                buffer.len(),
                timestamp,
                |length| Pin::new(&mut this.stream).poll_write(context, &buffer[..length]),
            )?;
            match attempt {
                WriteAttempt::Socket(result) => {
                    if let Poll::Ready(Ok(written)) = result {
                        this.node.metrics.wire(written);
                        this.node.governor.record_egress(
                            || {
                                this.node.access.egress_owner(
                                    this.origin.share,
                                    this.origin.bearer.as_ref(),
                                    this.origin
                                        .usage
                                        .attribution()
                                        .or_else(|| this.origin.share_attribution.get().copied()),
                                ) == Some(true)
                            },
                            written as u64,
                        );
                        if this.node.usage.is_configured() {
                            this.origin.usage.record(
                                &this.node.usage,
                                written,
                                timestamp.unsigned_abs(),
                            );
                        }
                    }
                    return result;
                }
                WriteAttempt::Refill => {
                    let node = this.node.clone();
                    let origin = this.origin.clone();
                    let requested = buffer.len() as u64;
                    this.refill = Some(tokio::task::spawn_blocking(move || {
                        node.refill_budgets(&origin, requested, super::now())
                    }));
                }
            }
        }
    }

    fn poll_flush(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_flush(context)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_shutdown(context)
    }
}
