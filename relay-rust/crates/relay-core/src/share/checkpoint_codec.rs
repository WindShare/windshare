use super::{Checkpoint, ConnectionCheckpoint, ConnectionRef, Error, Identity, Limits};
use super::{handshake_checkpoint::HandshakeCheckpoint, restoration::ConnectionRole};
use crate::lifecycle::Incarnation;
use crate::{
    flow::LedgerCheckpoint,
    identifiers::{RelayIdentity, ShareId},
    sessions::SessionSnapshot,
    wire::Reader,
};
use alloc::vec::Vec;

const FORMAT: u8 = 1;
impl Checkpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(FORMAT);
        out.extend_from_slice(self.identity.relay.as_bytes());
        out.extend_from_slice(self.identity.share.as_bytes());
        out.extend_from_slice(self.identity.incarnation.as_bytes());
        out.extend_from_slice(&self.last_session.to_be_bytes());
        out.extend_from_slice(&(self.connections.len() as u64).to_be_bytes());
        for connection in &self.connections {
            out.extend_from_slice(&connection.connection.encode());
            match &connection.role {
                ConnectionRole::Established => out.push(0),
                ConnectionRole::Closing => out.push(1),
                ConnectionRole::Handshake(handshake) => {
                    out.push(2);
                    block(&mut out, &handshake.encode());
                }
            }
            block(&mut out, &connection.ledger.encode());
        }
        out.extend_from_slice(&(self.sessions.len() as u64).to_be_bytes());
        for session in &self.sessions {
            block(&mut out, session.encode().as_bytes());
        }
        out
    }

    /// Limits are checked before allocating collections or nested ledger records.
    ///
    /// # Errors
    /// Rejects malformed or oversized checkpoint data. Restore additionally checks authority.
    pub fn decode(bytes: &[u8], limits: Limits, flow_slots: usize) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let relay = RelayIdentity::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let share = ShareId::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let incarnation = Incarnation::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let last_session = u64::from_be_bytes(field(&mut body)?);
        let count = bounded_count(&mut body, limits.connections)?;
        let mut connections = Vec::with_capacity(count);
        for _ in 0..count {
            let connection =
                ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
            let role = match field::<1>(&mut body)? {
                [0] => ConnectionRole::Established,
                [1] => ConnectionRole::Closing,
                [2] => ConnectionRole::Handshake(HandshakeCheckpoint::decode(read_block(
                    &mut body,
                    HandshakeCheckpoint::MAX_ENCODED_BYTES,
                )?)?),
                _ => return Err(Error::InvalidCheckpoint),
            };
            let maximum =
                LedgerCheckpoint::maximum_bytes(flow_slots).ok_or(Error::InvalidCheckpoint)?;
            let ledger = LedgerCheckpoint::decode(read_block(&mut body, maximum)?, flow_slots)
                .map_err(|_| Error::InvalidCheckpoint)?;
            connections.push(ConnectionCheckpoint {
                connection,
                role,
                ledger,
            });
        }
        let count = bounded_count(&mut body, limits.sessions)?;
        let mut sessions = Vec::with_capacity(count);
        for _ in 0..count {
            sessions.push(
                SessionSnapshot::decode(read_block(
                    &mut body,
                    crate::sessions::MAX_SNAPSHOT_BYTES,
                )?)
                .map_err(|_| Error::InvalidCheckpoint)?,
            );
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        Ok(Self {
            identity: Identity {
                relay,
                share,
                incarnation,
            },
            last_session,
            connections,
            sessions,
        })
    }
}
fn block(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    out.extend_from_slice(bytes);
}
fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], Error> {
    body.array().map_err(|_| Error::InvalidCheckpoint)
}
fn bounded_count(body: &mut Reader<'_>, maximum: usize) -> Result<usize, Error> {
    let value =
        usize::try_from(u64::from_be_bytes(field(body)?)).map_err(|_| Error::InvalidCheckpoint)?;
    if value > maximum || value > body.remainder().len() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(value)
}
fn read_block<'a>(body: &mut Reader<'a>, maximum: usize) -> Result<&'a [u8], Error> {
    let length = bounded_count(body, maximum)?;
    body.take(length).map_err(|_| Error::InvalidCheckpoint)
}
