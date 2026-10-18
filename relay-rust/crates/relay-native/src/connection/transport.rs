//! Field drop order keeps admission charged throughout transport destruction.
use super::Admission;
use futures_util::{Sink, Stream};
use std::{
    pin::Pin,
    sync::Arc,
    task::{Context, Poll},
};
use tokio_tungstenite::tungstenite::Message;

pub struct AdmittedSocket<S> {
    socket: S,
    _admission: Arc<Admission>,
}

impl<S> AdmittedSocket<S> {
    pub const fn new(socket: S, admission: Arc<Admission>) -> Self {
        Self {
            socket,
            _admission: admission,
        }
    }
}

impl<S: Stream + Unpin> Stream for AdmittedSocket<S> {
    type Item = S::Item;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        Pin::new(&mut self.socket).poll_next(context)
    }
}

impl<S: Sink<Message> + Unpin> Sink<Message> for AdmittedSocket<S> {
    type Error = S::Error;

    fn poll_ready(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Result<(), Self::Error>> {
        Pin::new(&mut self.socket).poll_ready(context)
    }

    fn start_send(mut self: Pin<&mut Self>, message: Message) -> Result<(), Self::Error> {
        Pin::new(&mut self.socket).start_send(message)
    }

    fn poll_flush(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Result<(), Self::Error>> {
        Pin::new(&mut self.socket).poll_flush(context)
    }

    fn poll_close(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Result<(), Self::Error>> {
        Pin::new(&mut self.socket).poll_close(context)
    }
}

#[cfg(test)]
#[path = "transport_tests.rs"]
mod tests;
