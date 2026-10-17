//! `REGISTER` and `RESUME`: how a sender gains authority to route a share
//! through this relay.
//!
//! A fresh registration answers a challenge and then uploads the descriptor it
//! committed by digest. A resume presents the committed resume token before its
//! challenge, then proves the same sender key without uploading again.

use crate::{
    authentication::{AuthenticationError, SenderProof},
    challenge::{Challenge, ChallengePurpose},
    clock::UnixMillis,
    descriptor_upload::{AuthorizedDescriptor, DescriptorDigestMismatch, DescriptorUpload},
    flow::ReceiveCapacity,
    identifiers::{
        DescriptorDigest, RelayIdentity, ResumeToken, ResumeTokenHash, ShareId, ShareInstance,
        ShareRoute,
    },
    wire::{FrameError, Magic, Writer, open_discriminated, open_reserved},
};

pub(crate) const REGISTER_INIT_MAGIC: Magic = *b"WS2R";
const REGISTER_PROOF_MAGIC: Magic = *b"WS2P";
const RESUME_CREDENTIAL_MAGIC: Magic = *b"WS2T";
const REGISTERED_MAGIC: Magic = *b"WS2K";

const REGISTER_DOMAIN: &[u8] = b"windshare/v2 relay-register\0";
const RESUME_DOMAIN: &[u8] = b"windshare/v2 relay-resume\0";
// Share ID, instance, pkHash, descriptor digest, resume token hash, relay
// identity, then the challenge ID, nonce, big-endian expiry and receive capacity.
const SIGNED_FIELD_BYTES: usize =
    12 + 16 + 16 + 32 + 32 + 32 + 16 + 32 + 8 + ReceiveCapacity::ENCODED_BYTES;
const PREIMAGE_CAPACITY: usize = REGISTER_DOMAIN.len() + SIGNED_FIELD_BYTES;
const _: () = assert!(RESUME_DOMAIN.len() <= REGISTER_DOMAIN.len());

/// Whether a registration creates a route or takes over an existing one.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum RegistrationMode {
    /// Publishes a new route and uploads its descriptor.
    Fresh,
    /// Reclaims a route this sender registered before.
    Resume,
}

impl RegistrationMode {
    const fn from_wire(discriminator: u8) -> Result<Self, FrameError> {
        match discriminator {
            0 => Ok(Self::Fresh),
            1 => Ok(Self::Resume),
            _ => Err(FrameError::Discriminator),
        }
    }

    /// Returns the only challenge purpose a proof in this mode may answer.
    #[must_use]
    pub const fn challenge_purpose(self) -> ChallengePurpose {
        match self {
            Self::Fresh => ChallengePurpose::Register,
            Self::Resume => ChallengePurpose::Resume,
        }
    }

    // Distinct domains keep a fresh signature from authorizing a resume.
    const fn domain(self) -> &'static [u8] {
        match self {
            Self::Fresh => REGISTER_DOMAIN,
            Self::Resume => RESUME_DOMAIN,
        }
    }
}

/// What a registration commits, independent of whether it is fresh or a
/// resume: these are the facts a relay publishes and later restores.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Registration {
    route: ShareRoute,
    share_instance: ShareInstance,
    descriptor_digest: DescriptorDigest,
    resume_token_hash: ResumeTokenHash,
}

impl Registration {
    pub(crate) const fn new(
        route: ShareRoute,
        share_instance: ShareInstance,
        descriptor_digest: DescriptorDigest,
        resume_token_hash: ResumeTokenHash,
    ) -> Self {
        Self {
            route,
            share_instance,
            descriptor_digest,
            resume_token_hash,
        }
    }

    /// Returns the share route whose key hash every proof must match.
    #[must_use]
    pub const fn route(&self) -> ShareRoute {
        self.route
    }

    /// Returns the content instance the registration serves.
    #[must_use]
    pub const fn share_instance(&self) -> ShareInstance {
        self.share_instance
    }

    /// Returns the digest the uploaded descriptor bytes must match.
    #[must_use]
    pub const fn descriptor_digest(&self) -> DescriptorDigest {
        self.descriptor_digest
    }

    /// Returns the commitment a later resume token must hash to.
    #[must_use]
    pub const fn resume_token_hash(&self) -> ResumeTokenHash {
        self.resume_token_hash
    }
}

/// `REGISTER_INIT` (`WS2R`): the share identity a sender asks to route.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RegisterInit {
    mode: RegistrationMode,
    registration: Registration,
    receive_capacity: ReceiveCapacity,
}

impl RegisterInit {
    pub const ENCODED_BYTES: usize = 116 + ReceiveCapacity::ENCODED_BYTES;

    #[must_use]
    pub fn encode(&self) -> [u8; Self::ENCODED_BYTES] {
        let mut out = Writer::with_header(
            REGISTER_INIT_MAGIC,
            match self.mode {
                RegistrationMode::Fresh => 0,
                RegistrationMode::Resume => 1,
            },
        );
        out.put(self.registration.route.share_id().as_bytes())
            .put(self.registration.share_instance.as_bytes())
            .put(self.registration.route.pk_hash().as_bytes())
            .put(self.registration.descriptor_digest.as_bytes())
            .put(self.registration.resume_token_hash.as_bytes());
        self.receive_capacity.put(&mut out);
        out.into_frame()
    }

    /// Decodes one complete `WS2R` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout, a
    /// known mode, a nonzero share instance and a share ID derived from its
    /// key hash.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let (mode, mut body) = open_discriminated(encoded, REGISTER_INIT_MAGIC)?;
        let mode = RegistrationMode::from_wire(mode)?;
        let share_id = body.array()?;
        let share_instance = body.array()?;
        let pk_hash = body.array()?;
        let descriptor_digest = body.array()?;
        let resume_token_hash = body.array()?;
        let receive_capacity = ReceiveCapacity::read(&mut body)?;
        body.finish()?;
        Ok(Self {
            mode,
            receive_capacity,
            registration: Registration::new(
                ShareRoute::from_wire(share_id, pk_hash)?,
                ShareInstance::new(share_instance).ok_or(FrameError::Field)?,
                DescriptorDigest::new(descriptor_digest),
                ResumeTokenHash::new(resume_token_hash),
            ),
        })
    }

    /// Returns whether this registration is fresh or a resume.
    #[must_use]
    pub const fn mode(&self) -> RegistrationMode {
        self.mode
    }

    /// Returns the facts this request asks the relay to publish or reclaim.
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }

    /// This connection's receive bound; it is not part of durable share ownership.
    #[must_use]
    pub const fn receive_capacity(&self) -> ReceiveCapacity {
        self.receive_capacity
    }

    fn preimage(&self, challenge: &Challenge, relay: &RelayIdentity) -> Writer<PREIMAGE_CAPACITY> {
        let registration = &self.registration;
        let mut preimage = Writer::new();
        preimage
            .put(self.mode.domain())
            .put(registration.route.share_id().as_bytes())
            .put(registration.share_instance.as_bytes())
            .put(registration.route.pk_hash().as_bytes())
            .put(registration.descriptor_digest.as_bytes())
            .put(registration.resume_token_hash.as_bytes())
            .put(relay.as_bytes());
        challenge.put_body(&mut preimage);
        self.receive_capacity.put(&mut preimage);
        preimage
    }
}

/// `REGISTER_PROOF` (`WS2P`): the sender's answer to a registration challenge.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RegisterProof {
    mode: RegistrationMode,
    proof: SenderProof,
}

impl RegisterProof {
    /// Decodes one complete `WS2P` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout, a
    /// known mode, an accepted sender key and a nonzero signature.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let (mode, mut body) = open_discriminated(encoded, REGISTER_PROOF_MAGIC)?;
        let mode = RegistrationMode::from_wire(mode)?;
        let proof = SenderProof::read(&mut body)?;
        body.finish()?;
        Ok(Self { mode, proof })
    }

    /// Returns the mode the sender claims to be proving.
    #[must_use]
    pub const fn mode(&self) -> RegistrationMode {
        self.mode
    }

    /// Returns the claimed sender key and signature.
    #[must_use]
    pub const fn sender_proof(&self) -> &SenderProof {
        &self.proof
    }
}

/// `RESUME_CREDENTIAL` (`WS2T`): the resume token presented before a challenge.
///
/// Hosts compare its hash with the resume commitment before issuing a
/// challenge, so a wrong token never spends challenge capacity.
#[derive(Debug)]
pub struct ResumeCredential {
    token: ResumeToken,
}

impl ResumeCredential {
    /// Decodes one complete `WS2T` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout and a
    /// nonzero token.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, RESUME_CREDENTIAL_MAGIC)?;
        let token = ResumeToken::new(body.array()?).ok_or(FrameError::Field)?;
        body.finish()?;
        Ok(Self { token })
    }

    /// Returns the presented token.
    #[must_use]
    pub const fn token(&self) -> &ResumeToken {
        &self.token
    }
}

/// `REGISTERED` (`WS2K`): acknowledges that a registration is routable.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Registered {
    share_id: ShareId,
    share_instance: ShareInstance,
    descriptor_digest: DescriptorDigest,
}

impl Registered {
    /// Encoded `WS2K` width in bytes.
    pub const ENCODED_BYTES: usize = 68;

    /// Acknowledges exactly the identity and descriptor `registration` committed.
    #[must_use]
    pub const fn new(registration: &Registration) -> Self {
        Self {
            share_id: registration.route.share_id(),
            share_instance: registration.share_instance,
            descriptor_digest: registration.descriptor_digest,
        }
    }

    /// Encodes the acknowledgement sent to the sender.
    #[must_use]
    pub fn encode(&self) -> [u8; Self::ENCODED_BYTES] {
        let mut frame = Writer::with_header(REGISTERED_MAGIC, 0);
        frame
            .put(self.share_id.as_bytes())
            .put(self.share_instance.as_bytes())
            .put(self.descriptor_digest.as_bytes());
        frame.into_frame()
    }
}

/// Authenticates a `REGISTER` or `RESUME` proof against the challenge this
/// relay issued for `init`.
///
/// The challenge is consumed whatever the outcome, so a failed proof cannot
/// turn one challenge into repeated signature or key-validity probes.
///
/// # Errors
///
/// Returns an [`AuthenticationError`] when the challenge purpose or proof mode
/// differs from `init`, the challenge is no longer live at `now`, the proof key
/// is not the share's key, or the signature does not cover this exact
/// registration, relay and challenge.
#[expect(
    clippy::needless_pass_by_value,
    reason = "consuming the challenge enforces its one-use rule"
)]
pub fn authenticate_registration(
    init: RegisterInit,
    challenge: Challenge,
    relay: &RelayIdentity,
    proof: &RegisterProof,
    now: UnixMillis,
) -> Result<RegistrationAuthority, AuthenticationError> {
    if challenge.purpose() != init.mode.challenge_purpose() {
        return Err(AuthenticationError::PurposeMismatch);
    }
    if proof.mode != init.mode {
        return Err(AuthenticationError::ModeMismatch);
    }
    if !challenge.is_live_at(now) {
        return Err(AuthenticationError::ChallengeExpired);
    }
    proof.proof.authenticate(
        init.registration.route.pk_hash(),
        init.preimage(&challenge, relay).written(),
    )?;
    Ok(match init.mode {
        RegistrationMode::Fresh => RegistrationAuthority::Fresh(FreshRegistration { init }),
        RegistrationMode::Resume => RegistrationAuthority::Resume(ResumeRegistration { init }),
    })
}

/// Authority a sender proved for one registration on this relay.
///
/// Authorities are move-only capabilities: only successful authentication
/// mints them, and the publication they permit consumes them.
#[derive(Debug, Eq, PartialEq)]
pub enum RegistrationAuthority {
    /// May publish the committed descriptor under a new route.
    Fresh(FreshRegistration),
    /// May take over the existing route without uploading again.
    Resume(ResumeRegistration),
}

/// Authority to publish the descriptor a fresh registration committed.
#[derive(Debug, Eq, PartialEq)]
pub struct FreshRegistration {
    init: RegisterInit,
}

impl FreshRegistration {
    /// Returns the authenticated registration.
    #[must_use]
    pub const fn init(&self) -> &RegisterInit {
        &self.init
    }

    /// Admits the upload only when its bytes hash to the committed digest.
    ///
    /// The relay never parses the sealed object, so object formats can evolve
    /// end to end without changing relay admission.
    ///
    /// # Errors
    ///
    /// Returns [`DescriptorDigestMismatch`] when the upload is not the
    /// committed descriptor; the authority is spent either way.
    pub fn authorize_descriptor(
        self,
        upload: DescriptorUpload<'_>,
    ) -> Result<AuthorizedDescriptor<'_>, DescriptorDigestMismatch> {
        if DescriptorDigest::of(upload.object()) != self.init.registration.descriptor_digest {
            return Err(DescriptorDigestMismatch);
        }
        Ok(AuthorizedDescriptor::new(self, upload.object()))
    }
}

/// Authority to take over the route a resume registration names.
#[derive(Debug, Eq, PartialEq)]
pub struct ResumeRegistration {
    init: RegisterInit,
}

impl ResumeRegistration {
    /// Returns the authenticated registration.
    #[must_use]
    pub const fn init(&self) -> &RegisterInit {
        &self.init
    }
}

#[cfg(test)]
#[path = "registration_tests.rs"]
mod tests;
