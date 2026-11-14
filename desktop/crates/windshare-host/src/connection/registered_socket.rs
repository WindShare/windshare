use polling::{Event, Poller};
use std::{io, mem::ManuallyDrop, net::TcpStream};
use tungstenite::{Message, WebSocket};

#[cfg(test)]
#[path = "registered_socket_tests.rs"]
mod tests;

const CONTROL_SOCKET_KEY: usize = 0;

/// Owns the socket until the poller has relinquished it. Only protocol operations
/// are exposed: replacing or extracting the registered stream would bypass cleanup.
pub(super) struct RegisteredSocket<'poller> {
    socket: Option<ManuallyDrop<WebSocket<TcpStream>>>,
    poller: &'poller Poller,
    session_id: &'poller str,
}

impl<'poller> RegisteredSocket<'poller> {
    pub(super) fn new(
        socket: WebSocket<TcpStream>,
        poller: &'poller Poller,
        session_id: &'poller str,
    ) -> io::Result<Self> {
        // Install ownership before add: a backend may register the source and
        // then fail while arming it. Unwinding must also attempt removal.
        let mut registered = Self {
            socket: Some(ManuallyDrop::new(socket)),
            poller,
            session_id,
        };
        // SAFETY: registered owns the live socket and never exposes its stream.
        // Drop removes the registration before closing it, including add errors
        // and unwinding. Failed removal retains the allocation and open handle.
        unsafe {
            poller.add(
                registered.socket().get_ref(),
                Event::readable(CONTROL_SOCKET_KEY),
            )?;
        }
        tracing::debug!(event = "desktop.control_socket_registered", %session_id);
        Ok(registered)
    }

    pub(super) fn read(&mut self) -> tungstenite::Result<Message> {
        self.socket().read()
    }

    pub(super) fn send(&mut self, message: Message) -> tungstenite::Result<()> {
        self.socket().send(message)
    }

    pub(super) fn flush(&mut self) -> tungstenite::Result<()> {
        self.socket().flush()
    }

    pub(super) fn rearm(&mut self, writable: bool) -> io::Result<()> {
        let poller = self.poller;
        poller.modify(
            self.socket().get_ref(),
            Event::new(CONTROL_SOCKET_KEY, true, writable),
        )
    }

    pub(super) fn close(mut self) -> io::Result<()> {
        self.unregister().map(drop)
    }

    fn socket(&mut self) -> &mut WebSocket<TcpStream> {
        self.socket.as_mut().expect("registered socket is owned")
    }

    fn unregister(&mut self) -> io::Result<Option<WebSocket<TcpStream>>> {
        // Taking the owner first prevents a second removal during unwinding.
        let Some(socket) = self.socket.take() else {
            return Ok(None);
        };
        // ManuallyDrop also retains the socket if deletion itself panics.
        if let Err(error) = self.poller.delete(socket.get_ref()) {
            tracing::error!(
                event = "desktop.control_socket_unregister_failed",
                session_id = %self.session_id,
                socket_retained = true,
                %error,
            );
            return Err(error);
        }
        tracing::debug!(
            event = "desktop.control_socket_unregistered",
            session_id = %self.session_id,
        );
        Ok(Some(ManuallyDrop::into_inner(socket)))
    }
}

impl Drop for RegisteredSocket<'_> {
    fn drop(&mut self) {
        let _ = self.unregister();
    }
}
