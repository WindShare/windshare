//! `STOP`: how a sender permanently retires a share instance on this relay.
//!
//! STOP proofs sign the target relay identity inside the init frame, so a
//! proof obtained for one relay can never stop the same share on another.

use crate::{
    authentication::{AuthenticationError, SenderProof},
    challenge::{Challenge, ChallengePurpose},
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareInstance, ShareRoute, StopId},
    wire::{FrameError, Magic, Writer, open_reserved},
};

pub(crate) const STOP_INIT_MAGIC: Magic = *b"WS2X";
const STOP_PROOF_MAGIC: Magic = *b"WS2V";
const STOPPED_MAGIC: Magic = *b"WS2Y";

const STOP_DOMAIN: &[u8] = b"windshare/v2 relay-stop\0";
// Share ID, instance, pkHash, relay identity, stop ID, then the challenge ID,
// nonce and big-endian expiry.
const SIGNED_FIELD_BYTES: usize = 12 + 16 + 16 + 32 + 16 + 16 + 32 + 8;
const PREIMAGE_BYTES: usize = STOP_DOMAIN.len() + SIGNED_FIELD_BYTES;

/// `STOP_INIT` (`WS2X`): the share instance a sender asks one relay to retire.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StopInit {
    route: ShareRoute,
    share_instance: ShareInstance,
    relay: RelayIdentity,
    stop_id: StopId,
}

impl StopInit {
    /// Decodes one complete `WS2X` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout,
    /// nonzero instance, relay and stop identities, and a share ID derived
    /// from its key hash.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, STOP_INIT_MAGIC)?;
        let share_id = body.array()?;
        let share_instance = body.array()?;
        let pk_hash = body.array()?;
        let relay = body.array()?;
        let stop_id = body.array()?;
        body.finish()?;
        Ok(Self {
            route: ShareRoute::from_wire(share_id, pk_hash)?,
            share_instance: ShareInstance::new(share_instance).ok_or(FrameError::Field)?,
            relay: RelayIdentity::new(relay).ok_or(FrameError::Field)?,
            stop_id: StopId::new(stop_id).ok_or(FrameError::Field)?,
        })
    }

    /// Returns the share route whose key hash the proof must match.
    #[must_use]
    pub const fn route(&self) -> ShareRoute {
        self.route
    }

    /// Returns the content instance to retire.
    #[must_use]
    pub const fn share_instance(&self) -> ShareInstance {
        self.share_instance
    }

    /// Returns the relay the sender addressed.
    #[must_use]
    pub const fn relay(&self) -> RelayIdentity {
        self.relay
    }

    /// Returns the identifier STOPPED must echo.
    #[must_use]
    pub const fn stop_id(&self) -> StopId {
        self.stop_id
    }

    fn preimage(&self, challenge: &Challenge) -> Writer<PREIMAGE_BYTES> {
        let mut preimage = Writer::new();
        preimage
            .put(STOP_DOMAIN)
            .put(self.route.share_id().as_bytes())
            .put(self.share_instance.as_bytes())
            .put(self.route.pk_hash().as_bytes())
            .put(self.relay.as_bytes())
            .put(self.stop_id.as_bytes());
        challenge.put_body(&mut preimage);
        preimage
    }
}

/// `STOP_PROOF` (`WS2V`): the sender's answer to a STOP challenge.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct StopProof {
    proof: SenderProof,
}

impl StopProof {
    /// Decodes one complete `WS2V` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout, an
    /// accepted sender key and a nonzero signature.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, STOP_PROOF_MAGIC)?;
        let proof = SenderProof::read(&mut body)?;
        body.finish()?;
        Ok(Self { proof })
    }

    /// Returns the claimed sender key and signature.
    #[must_use]
    pub const fn sender_proof(&self) -> &SenderProof {
        &self.proof
    }
}

/// `STOPPED` (`WS2Y`): confirms that a STOP is durably committed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Stopped {
    stop_id: StopId,
}

impl Stopped {
    /// Encoded `WS2Y` width in bytes.
    pub const ENCODED_BYTES: usize = 24;

    /// Acknowledges the STOP request that carried `stop_id`.
    #[must_use]
    pub const fn new(stop_id: StopId) -> Self {
        Self { stop_id }
    }

    /// Encodes the confirmation sent to the sender.
    #[must_use]
    pub fn encode(&self) -> [u8; Self::ENCODED_BYTES] {
        let mut frame = Writer::with_header(STOPPED_MAGIC, 0);
        frame.put(self.stop_id.as_bytes());
        frame.into_frame()
    }
}

/// Authenticates a STOP proof against the challenge this relay issued.
///
/// The challenge is consumed whatever the outcome, so a failed proof cannot
/// turn one challenge into repeated signature or key-validity probes.
///
/// # Errors
///
/// Returns an [`AuthenticationError`] when the challenge was not issued for
/// STOP, `init` names another relay, the challenge is no longer live at `now`,
/// the proof key is not the share's key, or the signature does not cover this
/// exact STOP and challenge.
#[expect(
    clippy::needless_pass_by_value,
    reason = "consuming the challenge enforces its one-use rule"
)]
pub fn authenticate_stop(
    init: StopInit,
    challenge: Challenge,
    relay: &RelayIdentity,
    proof: &StopProof,
    now: UnixMillis,
) -> Result<StopAuthority, AuthenticationError> {
    if challenge.purpose() != ChallengePurpose::Stop {
        return Err(AuthenticationError::PurposeMismatch);
    }
    if init.relay != *relay {
        return Err(AuthenticationError::RelayMismatch);
    }
    if !challenge.is_live_at(now) {
        return Err(AuthenticationError::ChallengeExpired);
    }
    proof
        .proof
        .authenticate(init.route.pk_hash(), init.preimage(&challenge).written())?;
    Ok(StopAuthority { init })
}

/// Authority a sender proved to retire one share instance on this relay.
///
/// Move-only: only successful authentication mints it, and committing the STOP
/// consumes it.
#[derive(Debug, Eq, PartialEq)]
pub struct StopAuthority {
    init: StopInit,
}

impl StopAuthority {
    /// Returns the authenticated STOP request.
    #[must_use]
    pub const fn init(&self) -> &StopInit {
        &self.init
    }
}

#[cfg(test)]
#[path = "stop_tests.rs"]
mod tests;
