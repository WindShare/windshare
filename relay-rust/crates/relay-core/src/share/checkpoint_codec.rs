use super::{
    Checkpoint, CheckpointManifest, ConnectionCheckpoint, ConnectionRef, Error, Identity, Limits,
};
use super::{checkpoint::ConnectionRole, handshake_checkpoint::HandshakeCheckpoint};
use crate::{
    flow::LedgerCheckpoint,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::Incarnation,
    sessions::SessionSnapshot,
    wire::Reader,
};
use alloc::vec::Vec;

const FORMAT: u8 = 6;
const CONNECTION_FORMAT: u8 = 4;
const LENGTH_BYTES: usize = size_of::<u64>();
const IDENTITY_BYTES: usize = 32 + 12 + 16;
const CONNECTION_HEADER_BYTES: usize =
    1 + IDENTITY_BYTES + ConnectionRef::ENCODED_BYTES + 1 + 2 * LENGTH_BYTES;

impl ConnectionCheckpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(CONNECTION_FORMAT);
        put_identity(&mut out, self.identity);
        out.extend_from_slice(&self.connection.encode());
        match &self.role {
            ConnectionRole::Established => out.push(0),
            ConnectionRole::Closing => out.push(1),
            ConnectionRole::Detached(super::Detached::AwaitingRelease) => out.push(3),
            ConnectionRole::Detached(super::Detached::Released) => out.push(6),
            ConnectionRole::Detached(super::Detached::Quiescing { not_before }) => {
                out.push(4);
                out.extend_from_slice(&not_before.0.to_be_bytes());
            }
            ConnectionRole::Detached(super::Detached::Retired) => out.push(5),
            ConnectionRole::Handshake(handshake) => {
                out.push(2);
                block(&mut out, &handshake.encode());
            }
        }
        block(&mut out, &self.ledger.encode());
        out.extend_from_slice(&(self.sessions.len() as u64).to_be_bytes());
        for session in &self.sessions {
            block(&mut out, session.encode().as_bytes());
        }
        out
    }

    /// Maximum binary size for this configured retained-flow bound.
    /// A handshake cannot own flows/admissions; established sockets may retain
    /// the full sender fanout, including retired delivery obligations.
    #[must_use]
    pub fn maximum_bytes(retained_flow_limit: usize) -> Option<usize> {
        let established = LedgerCheckpoint::maximum_bytes(retained_flow_limit)?.checked_add(
            retained_flow_limit.checked_mul(LENGTH_BYTES + crate::sessions::MAX_SNAPSHOT_BYTES)?,
        )?;
        let handshake = LedgerCheckpoint::maximum_bytes(0)?
            .checked_add(LENGTH_BYTES + HandshakeCheckpoint::MAX_ENCODED_BYTES)?;
        let detached =
            LedgerCheckpoint::maximum_bytes(retained_flow_limit)?.checked_add(LENGTH_BYTES)?;
        CONNECTION_HEADER_BYTES.checked_add(established.max(handshake).max(detached))
    }

    /// # Errors
    /// Rejects oversized, malformed or misassigned records before recovery authorizes them.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let maximum = Self::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        if bytes.len() > maximum {
            return Err(Error::InvalidCheckpoint);
        }
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [CONNECTION_FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let identity = read_identity(&mut body)?;
        let connection =
            ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let role = match field::<1>(&mut body)? {
            [0] => ConnectionRole::Established,
            [1] => ConnectionRole::Closing,
            [3] => ConnectionRole::Detached(super::Detached::AwaitingRelease),
            [6] => ConnectionRole::Detached(super::Detached::Released),
            [4] => ConnectionRole::Detached(super::Detached::Quiescing {
                not_before: crate::clock::UnixMillis(u64::from_be_bytes(field(&mut body)?)),
            }),
            [5] => ConnectionRole::Detached(super::Detached::Retired),
            [2] => ConnectionRole::Handshake(HandshakeCheckpoint::decode(read_block(
                &mut body,
                HandshakeCheckpoint::MAX_ENCODED_BYTES,
            )?)?),
            _ => return Err(Error::InvalidCheckpoint),
        };
        let maximum = LedgerCheckpoint::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        let ledger = LedgerCheckpoint::decode(
            read_block(&mut body, maximum)?,
            limits.retained_flows_per_connection,
        )
        .map_err(|_| Error::InvalidCheckpoint)?;
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
        let checkpoint = Self {
            identity,
            connection,
            role,
            ledger,
            sessions,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }
}

impl Checkpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(FORMAT);
        block(&mut out, &self.manifest.encode());
        out.extend_from_slice(&(self.connections.len() as u64).to_be_bytes());
        for connection in &self.connections {
            block(&mut out, &connection.encode());
        }
        out
    }

    /// # Errors
    /// Rejects malformed, mixed or oversized collections. Restore additionally checks authority.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let manifest = CheckpointManifest::decode(read_block(&mut body, bytes.len())?, limits)?;
        let count = bounded_count(&mut body, limits.generation_limit()?)?;
        let mut connections = Vec::with_capacity(count);
        let maximum = ConnectionCheckpoint::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        for _ in 0..count {
            connections.push(ConnectionCheckpoint::decode(
                read_block(&mut body, maximum)?,
                limits,
            )?);
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let checkpoint = Self {
            manifest,
            connections,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }
}

pub(super) fn put_header(out: &mut Vec<u8>, identity: Identity, last_session: u64) {
    put_identity(out, identity);
    out.extend_from_slice(&last_session.to_be_bytes());
}
fn put_identity(out: &mut Vec<u8>, identity: Identity) {
    out.extend_from_slice(identity.relay.as_bytes());
    out.extend_from_slice(identity.share.as_bytes());
    out.extend_from_slice(identity.incarnation.as_bytes());
}
pub(super) fn read_header(body: &mut Reader<'_>) -> Result<(Identity, u64), Error> {
    Ok((read_identity(body)?, u64::from_be_bytes(field(body)?)))
}
fn read_identity(body: &mut Reader<'_>) -> Result<Identity, Error> {
    let relay = RelayIdentity::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    let share = ShareId::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    let incarnation = Incarnation::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    Ok(Identity {
        relay,
        share,
        incarnation,
    })
}
fn block(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    out.extend_from_slice(bytes);
}
pub(super) fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], Error> {
    body.array().map_err(|_| Error::InvalidCheckpoint)
}
pub(super) fn bounded_count(body: &mut Reader<'_>, maximum: usize) -> Result<usize, Error> {
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
