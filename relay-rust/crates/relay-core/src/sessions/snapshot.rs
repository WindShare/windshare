//! Inert session records that let an actor rebuilt over surviving sockets,
//! such as hibernated ones, reinstall the same admissions.

use core::{fmt, num::NonZeroU64};

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, RelaySessionId, ShareId, ShareInstance},
    lifecycle::{ConnectionRef, Incarnation, OwnershipEpoch, OwnershipId, RecordError},
    wire::{Reader, Writer},
};

use super::{Phase, SessionKey, Stage};

/// Snapshot layout version. Decoders refuse every other value, so a future
/// layout can never be misread as this one.
const FORMAT: u8 = 1;

const AWAITING_RECEIVER: u8 = 1;
const AWAITING_SENDER: u8 = 2;
const ACTIVE: u8 = 3;

// Format and stage.
const HEADER_BYTES: usize = 1 + 1;
// Relay, share, instance and wire ID; the owner's epoch and connection; then
// the admitting incarnation, receiver connection and admission sequence.
const KEY_BYTES: usize =
    32 + 12 + 16 + 8 + 8 + ConnectionRef::ENCODED_BYTES + 16 + ConnectionRef::ENCODED_BYTES + 8;
const DEADLINE_BYTES: usize = 8;

/// Largest encoded snapshot, a provisional session with its deadline; hosts
/// may size connection attachments from it.
pub const MAX_SNAPSHOT_BYTES: usize = HEADER_BYTES + KEY_BYTES + DEADLINE_BYTES;

/// A live session as its host records it beside the session's connections.
///
/// Snapshots grant nothing: hosts may store, copy or even forge them. Only
/// `ShareLifecycle::restore` checks one against the surviving ownership,
/// connections and sibling snapshots before reinstalling its session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SessionSnapshot {
    pub(super) key: SessionKey,
    pub(super) stage: Stage,
}

impl SessionSnapshot {
    /// The admission this snapshot claims.
    #[must_use]
    pub const fn key(&self) -> SessionKey {
        self.key
    }

    /// The recorded phase, which is never `Retired`.
    #[must_use]
    pub const fn phase(&self) -> Phase {
        self.stage.phase()
    }

    /// The original provisional deadline, which restoration never extends.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        self.stage.deadline()
    }

    /// Encodes the bytes a host records, at most [`MAX_SNAPSHOT_BYTES`].
    #[must_use]
    pub fn encode(&self) -> EncodedSnapshot {
        let (kind, deadline) = match self.stage {
            Stage::AwaitingReceiver { deadline } => (AWAITING_RECEIVER, Some(deadline)),
            Stage::AwaitingSender { deadline } => (AWAITING_SENDER, Some(deadline)),
            Stage::Active => (ACTIVE, None),
        };
        let key = self.key;
        let mut out = Writer::new();
        out.put(&[FORMAT, kind])
            .put(key.relay().as_bytes())
            .put(key.share().as_bytes())
            .put(key.instance.as_bytes())
            .put(key.wire_id.as_bytes())
            .put(&key.owner.epoch().get().get().to_be_bytes())
            .put(&key.sender().encode())
            .put(key.admitted_by.as_bytes())
            .put(&key.receiver.encode())
            .put(&key.admission_sequence.get().to_be_bytes());
        if let Some(deadline) = deadline {
            out.put(&deadline.0.to_be_bytes());
        }
        EncodedSnapshot(out)
    }

    /// Decodes bytes a host recorded from [`Self::encode`].
    ///
    /// # Errors
    ///
    /// Returns a [`RecordError`] unless the bytes have this format's exact
    /// layout for a known phase and nonzero identities. Decoding checks shape
    /// only; restoration decides whether the admission is still live.
    pub fn decode(encoded: &[u8]) -> Result<Self, RecordError> {
        let mut body = Reader::new(encoded);
        let [format, kind] = field(&mut body)?;
        if format != FORMAT {
            return Err(RecordError::Format);
        }
        let relay = valid(RelayIdentity::new(field(&mut body)?))?;
        let share = valid(ShareId::new(field(&mut body)?))?;
        let instance = valid(ShareInstance::new(field(&mut body)?))?;
        let wire_id = valid(RelaySessionId::new(field(&mut body)?))?;
        let epoch = OwnershipEpoch::new(nonzero(&mut body)?);
        let sender = connection(&mut body)?;
        let key = SessionKey {
            owner: OwnershipId::new(relay, share, epoch, sender),
            admitted_by: valid(Incarnation::new(field(&mut body)?))?,
            receiver: connection(&mut body)?,
            admission_sequence: nonzero(&mut body)?,
            wire_id,
            instance,
        };
        let stage = stage(kind, &mut body)?;
        body.finish().map_err(|_| RecordError::Length)?;
        Ok(Self { key, stage })
    }
}

/// The recorded bytes of one snapshot; their length depends on its phase.
pub struct EncodedSnapshot(Writer<MAX_SNAPSHOT_BYTES>);

impl EncodedSnapshot {
    /// Returns the bytes to record.
    #[must_use]
    pub fn as_bytes(&self) -> &[u8] {
        self.0.written()
    }
}

impl fmt::Debug for EncodedSnapshot {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_tuple("EncodedSnapshot")
            .field(&self.as_bytes())
            .finish()
    }
}

fn stage(kind: u8, body: &mut Reader<'_>) -> Result<Stage, RecordError> {
    let mut deadline = || field(body).map(|bytes| UnixMillis(u64::from_be_bytes(bytes)));
    Ok(match kind {
        AWAITING_RECEIVER => Stage::AwaitingReceiver {
            deadline: deadline()?,
        },
        AWAITING_SENDER => Stage::AwaitingSender {
            deadline: deadline()?,
        },
        ACTIVE => Stage::Active,
        _ => return Err(RecordError::Kind),
    })
}

fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], RecordError> {
    body.array().map_err(|_| RecordError::Length)
}

fn valid<T>(identity: Option<T>) -> Result<T, RecordError> {
    identity.ok_or(RecordError::Field)
}

fn nonzero(body: &mut Reader<'_>) -> Result<NonZeroU64, RecordError> {
    valid(NonZeroU64::new(u64::from_be_bytes(field(body)?)))
}

fn connection(body: &mut Reader<'_>) -> Result<ConnectionRef, RecordError> {
    valid(ConnectionRef::decode(field(body)?))
}
