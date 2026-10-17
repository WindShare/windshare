//! Pending handshakes are restored by replaying bounded control metadata. Proofs
//! are verified again; a serialized phase can never manufacture authenticated authority.
use super::{Error, Identity};
use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::{ConnectionRef, OwnershipEpoch, OwnershipId, Record},
    wire::Reader,
};
use alloc::vec::Vec;

const MAX_STEPS: usize = 4;
const MAX_FRAME_BYTES: usize = 256;

#[derive(Clone, Debug)]
pub(super) struct HandshakeCheckpoint {
    pub opened: UnixMillis,
    pub steps: Vec<Step>,
    pub binding: Binding,
}
#[derive(Clone, Debug)]
pub(super) enum Step {
    Frame { at: UnixMillis, bytes: Vec<u8> },
    Challenge { at: UnixMillis, bytes: [u8; 48] },
}
#[derive(Clone, Copy, Debug)]
pub(super) enum Binding {
    Unbound,
    Resume(Option<OwnershipId>),
}

impl HandshakeCheckpoint {
    pub const MAX_ENCODED_BYTES: usize = 8
        + 1
        + 2
        + crate::lifecycle::MAX_RECORD_BYTES
        + 1
        + MAX_STEPS * (1 + 8 + 2 + MAX_FRAME_BYTES);
    pub const fn new(opened: UnixMillis) -> Self {
        Self {
            opened,
            steps: Vec::new(),
            binding: Binding::Unbound,
        }
    }
    pub fn frame(&mut self, at: UnixMillis, bytes: &[u8]) -> Result<(), Error> {
        if self.steps.len() >= MAX_STEPS || bytes.len() > MAX_FRAME_BYTES {
            return Err(Error::InvalidCheckpoint);
        }
        self.steps.push(Step::Frame {
            at,
            bytes: bytes.to_vec(),
        });
        Ok(())
    }
    pub fn challenge(&mut self, at: UnixMillis, bytes: [u8; 48]) -> Result<(), Error> {
        if self.steps.len() >= MAX_STEPS {
            return Err(Error::InvalidCheckpoint);
        }
        self.steps.push(Step::Challenge { at, bytes });
        Ok(())
    }
    pub fn applicable(&self, stored: Option<Record>) -> bool {
        match self.binding {
            Binding::Unbound => true,
            Binding::Resume(observed) => observed == ownership(stored),
        }
    }
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.extend_from_slice(&self.opened.0.to_be_bytes());
        match self.binding {
            Binding::Unbound => out.push(0),
            Binding::Resume(None) => out.push(1),
            Binding::Resume(Some(owner)) => {
                out.push(2);
                out.extend_from_slice(owner.relay().as_bytes());
                out.extend_from_slice(owner.share().as_bytes());
                out.extend_from_slice(&owner.epoch().get().get().to_be_bytes());
                out.extend_from_slice(&owner.connection().encode());
            }
        }
        out.push(u8::try_from(self.steps.len()).expect("bounded transcript"));
        for step in &self.steps {
            match step {
                Step::Frame { at, bytes } => {
                    out.push(0);
                    out.extend_from_slice(&at.0.to_be_bytes());
                    put_block(&mut out, bytes);
                }
                Step::Challenge { at, bytes } => {
                    out.push(1);
                    out.extend_from_slice(&at.0.to_be_bytes());
                    out.extend_from_slice(bytes);
                }
            }
        }
        out
    }
    pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        let opened = UnixMillis(u64::from_be_bytes(field(&mut body)?));
        let binding = match field::<1>(&mut body)? {
            [0] => Binding::Unbound,
            [1] => Binding::Resume(None),
            [2] => {
                let relay =
                    RelayIdentity::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
                let share = ShareId::new(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
                let epoch = core::num::NonZeroU64::new(u64::from_be_bytes(field(&mut body)?))
                    .ok_or(Error::InvalidCheckpoint)?;
                let connection =
                    ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
                Binding::Resume(Some(OwnershipId::new(
                    relay,
                    share,
                    OwnershipEpoch::new(epoch),
                    connection,
                )))
            }
            _ => return Err(Error::InvalidCheckpoint),
        };
        let [count] = field(&mut body)?;
        if usize::from(count) > MAX_STEPS {
            return Err(Error::InvalidCheckpoint);
        }
        let mut steps = Vec::new();
        let mut previous = opened;
        for _ in 0..count {
            let [kind] = field(&mut body)?;
            let at = UnixMillis(u64::from_be_bytes(field(&mut body)?));
            if at < previous {
                return Err(Error::InvalidCheckpoint);
            }
            previous = at;
            steps.push(match kind {
                0 => Step::Frame {
                    at,
                    bytes: block(&mut body, MAX_FRAME_BYTES)?.to_vec(),
                },
                1 => Step::Challenge {
                    at,
                    bytes: field(&mut body)?,
                },
                _ => return Err(Error::InvalidCheckpoint),
            });
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        Ok(Self {
            opened,
            steps,
            binding,
        })
    }
    pub fn valid_at(&self, identity: Identity, now: UnixMillis) -> bool {
        self.opened <= now
            && self.steps.iter().all(|step| match step {
                Step::Frame { at, .. } | Step::Challenge { at, .. } => *at <= now,
            })
            && match self.binding {
                Binding::Resume(Some(record)) => {
                    record.relay() == identity.relay && record.share() == identity.share
                }
                _ => true,
            }
    }
}

pub(super) const fn ownership(record: Option<Record>) -> Option<OwnershipId> {
    match record {
        Some(Record::Published(owned)) => Some(owned.ownership()),
        Some(Record::Grace(grace)) => Some(grace.lost()),
        _ => None,
    }
}
fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], Error> {
    body.array().map_err(|_| Error::InvalidCheckpoint)
}
fn put_block(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(
        &u16::try_from(bytes.len())
            .expect("bounded metadata")
            .to_be_bytes(),
    );
    out.extend_from_slice(bytes);
}
fn block<'a>(body: &mut Reader<'a>, maximum: usize) -> Result<&'a [u8], Error> {
    let length = usize::from(u16::from_be_bytes(field(body)?));
    if length > maximum {
        return Err(Error::InvalidCheckpoint);
    }
    body.take(length).map_err(|_| Error::InvalidCheckpoint)
}
