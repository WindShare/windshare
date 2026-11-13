use super::{BudgetEpoch, Grant, Runtime};
use std::{
    future::Future,
    io,
    pin::Pin,
    task::{Context, Poll},
};
use tokio::{
    io::{AsyncRead, AsyncWrite, ReadBuf},
    task::JoinHandle,
};

pub struct Metered<S> {
    stream: S,
    node: Runtime,
    epoch: u64,
    credit: u64,
    budget_epoch: BudgetEpoch,
    reservation: Option<JoinHandle<anyhow::Result<Grant>>>,
}

impl<S> Metered<S> {
    pub fn new(stream: S, node: Runtime, epoch: u64) -> Self {
        Self {
            stream,
            budget_epoch: node.budget_epoch(),
            node,
            epoch,
            credit: 0,
            reservation: None,
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
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<io::Result<usize>> {
        if !self.node.available() || self.node.epoch() != self.epoch {
            return Poll::Ready(Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "relay temporarily unavailable",
            )));
        }
        if buffer.is_empty() {
            return Poll::Ready(Ok(0));
        }
        let budget_epoch = self.node.budget_epoch();
        if self.budget_epoch != budget_epoch {
            self.credit = 0;
            self.budget_epoch = budget_epoch;
        }
        if self.credit == 0 {
            if self.reservation.is_none() {
                let requested = buffer.len() as u64;
                if let Some(granted) = self.node.try_reserve(requested) {
                    self.budget_epoch = granted.epoch;
                    self.credit = granted.bytes;
                } else {
                    let node = self.node.clone();
                    self.reservation = Some(tokio::task::spawn_blocking(move || {
                        node.reserve_for_period(requested)
                    }));
                }
            }
            if let Some(reservation) = self.reservation.as_mut() {
                match Pin::new(reservation).poll(context) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(result) => {
                        self.reservation = None;
                        let granted = result
                            .map_err(io::Error::other)?
                            .map_err(io::Error::other)?;
                        if granted.epoch != self.node.budget_epoch() {
                            context.waker().wake_by_ref();
                            return Poll::Pending;
                        }
                        self.budget_epoch = granted.epoch;
                        self.credit = granted.bytes;
                    }
                }
            }
            if self.credit == 0 {
                return Poll::Ready(Err(io::Error::new(
                    io::ErrorKind::ConnectionAborted,
                    "relay budget exhausted",
                )));
            }
        }
        if self.budget_epoch != self.node.budget_epoch() {
            context.waker().wake_by_ref();
            return Poll::Pending;
        }
        let length = buffer
            .len()
            .min(usize::try_from(self.credit).unwrap_or(usize::MAX));
        match Pin::new(&mut self.stream).poll_write(context, &buffer[..length]) {
            Poll::Ready(Ok(written)) => {
                self.credit -= written as u64;
                self.node.metrics.wire(written);
                Poll::Ready(Ok(written))
            }
            result => result,
        }
    }

    fn poll_flush(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_flush(context)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_shutdown(context)
    }
}
