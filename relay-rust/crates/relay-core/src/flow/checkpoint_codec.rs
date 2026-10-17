use super::{
    CloseRequest, ConnectionState, Counts, FlowKey,
    checkpoint::{CheckpointError, LedgerCheckpoint},
    delivery::{Delivery, State},
};
use crate::{
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::ConnectionRef,
    sessions::{Direction, ForwardPermit, SessionSnapshot},
    wire::Reader,
};
use alloc::vec::Vec;

const FORMAT: u8 = 1;
const KEY_BYTES: usize = crate::sessions::MAX_SNAPSHOT_BYTES - size_of::<u64>();
const COUNTERS: usize = 5;
const ENTRY_BYTES: usize = 1 + KEY_BYTES + COUNTERS * 16 + 1 + 8;

impl LedgerCheckpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(FORMAT);
        out.push(match self.state {
            ConnectionState::Open => 0,
            ConnectionState::Closing(_) => 1,
            ConnectionState::Closed => 2,
        });
        out.extend_from_slice(self.relay.as_bytes());
        out.extend_from_slice(&self.destination.encode());
        if let ConnectionState::Closing(request) = self.state {
            out.extend_from_slice(&(request.flow.slot as u64).to_be_bytes());
            encode_permit(&mut out, request.flow);
            out.extend_from_slice(&request.deadline.0.to_be_bytes());
        }
        out.extend_from_slice(&(self.entries.len() as u64).to_be_bytes());
        for entry in &self.entries {
            out.push(u8::from(entry.is_some()));
            if let Some(entry) = entry {
                encode_permit(&mut out, entry.key);
                for count in [
                    entry.startup,
                    entry.granted,
                    entry.received,
                    entry.submitted,
                    entry.consumed,
                ] {
                    out.extend_from_slice(&count.frames.to_be_bytes());
                    out.extend_from_slice(&count.encoded_bytes.to_be_bytes());
                }
                let kind = match entry.state {
                    State::Open => 0,
                    State::Discarding { .. } => 1,
                    State::Settling { .. } => 2,
                };
                out.push(kind);
                out.extend_from_slice(&entry.deadline().map_or(0, |at| at.0).to_be_bytes());
            }
        }
        out
    }

    /// The configured slot bound is checked before allocating or reading entries.
    ///
    /// # Errors
    /// Rejects unknown layouts, truncated records, invalid identities and excessive slots.
    pub fn decode(bytes: &[u8], max_slots: usize) -> Result<Self, CheckpointError> {
        let mut body = Reader::new(bytes);
        let [format, kind] = field(&mut body)?;
        if format != FORMAT || kind > 2 {
            return Err(CheckpointError::Format);
        }
        let relay = RelayIdentity::new(field(&mut body)?).ok_or(CheckpointError::Identity)?;
        let destination =
            ConnectionRef::decode(field(&mut body)?).ok_or(CheckpointError::Identity)?;
        let state = match kind {
            0 => ConnectionState::Open,
            1 => {
                let slot = usize::try_from(u64::from_be_bytes(field(&mut body)?))
                    .map_err(|_| CheckpointError::Capacity)?;
                if slot >= max_slots {
                    return Err(CheckpointError::Capacity);
                }
                let flow = FlowKey {
                    slot,
                    permit: decode_permit(&mut body)?,
                };
                let deadline = UnixMillis(u64::from_be_bytes(field(&mut body)?));
                ConnectionState::Closing(CloseRequest {
                    destination,
                    flow,
                    deadline,
                })
            }
            _ => ConnectionState::Closed,
        };
        let count = usize::try_from(u64::from_be_bytes(field(&mut body)?))
            .map_err(|_| CheckpointError::Capacity)?;
        if count > max_slots || count > body.remainder().len() {
            return Err(CheckpointError::Capacity);
        }
        let mut entries = Vec::with_capacity(count);
        for slot in 0..count {
            entries.push(match field::<1>(&mut body)? {
                [0] => None,
                [1] => Some(decode_entry(&mut body, slot)?),
                _ => return Err(CheckpointError::Format),
            });
        }
        body.finish().map_err(|_| CheckpointError::Format)?;
        Ok(Self {
            relay,
            destination,
            entries,
            state,
        })
    }

    #[must_use]
    pub fn maximum_bytes(slots: usize) -> Option<usize> {
        slots
            .checked_mul(1 + ENTRY_BYTES)?
            .checked_add(2 + 32 + ConnectionRef::ENCODED_BYTES + 8)?
            .checked_add(8 + 1 + KEY_BYTES + 8)
    }
}

fn encode_permit(out: &mut Vec<u8>, flow: FlowKey) {
    out.push(match flow.direction() {
        Direction::ToSender => 0,
        Direction::ToReceiver => 1,
    });
    out.extend_from_slice(flow.session().checkpoint_key().encode().as_bytes());
}

fn decode_permit(body: &mut Reader<'_>) -> Result<ForwardPermit, CheckpointError> {
    let direction = match field::<1>(body)? {
        [0] => Direction::ToSender,
        [1] => Direction::ToReceiver,
        _ => return Err(CheckpointError::Format),
    };
    let snapshot = SessionSnapshot::decode(&field::<KEY_BYTES>(body)?)
        .map_err(|_| CheckpointError::Identity)?;
    Ok(ForwardPermit::restored(snapshot.key(), direction))
}

fn decode_entry(body: &mut Reader<'_>, slot: usize) -> Result<Delivery, CheckpointError> {
    let permit = decode_permit(body)?;
    let mut counts = [Counts::ZERO; COUNTERS];
    for count in &mut counts {
        *count = Counts::new(
            u64::from_be_bytes(field(body)?),
            u64::from_be_bytes(field(body)?),
        );
    }
    let [kind] = field(body)?;
    let deadline = UnixMillis(u64::from_be_bytes(field(body)?));
    let state = match kind {
        0 if deadline.0 == 0 => State::Open,
        1 => State::Discarding { deadline },
        2 => State::Settling { deadline },
        _ => return Err(CheckpointError::Format),
    };
    let [startup, granted, received, submitted, consumed] = counts;
    Ok(Delivery {
        key: FlowKey { slot, permit },
        startup,
        granted,
        received,
        submitted,
        consumed,
        state,
    })
}
fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], CheckpointError> {
    body.array().map_err(|_| CheckpointError::Format)
}
