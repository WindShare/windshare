use crate::store::{Store, load, save};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::trust::{Generation, TrustStore};

const SIGNING_GENERATION_KEY: &str = "signing-generation";

pub struct Signing {
    pub generation: Generation,
    pub key: SigningKey,
    pub certificate: String,
}
impl Signing {
    /// # Errors
    /// The online seed must match a certificate authenticated by the configured root.
    pub fn load(root: &str, certificate: &str, seed: &str) -> Result<Self> {
        let root = URL_SAFE_NO_PAD
            .decode(root)?
            .try_into()
            .map_err(|_| anyhow!("root width"))?;
        let generation = Generation::verify(&URL_SAFE_NO_PAD.decode(certificate)?, root)
            .map_err(|error| anyhow!("{error:?}"))?;
        let key = SigningKey::from_bytes(
            &URL_SAFE_NO_PAD
                .decode(seed)?
                .try_into()
                .map_err(|_| anyhow!("seed width"))?,
        );
        ensure!(
            key.verifying_key().to_bytes() == generation.key,
            "online signing key does not match certificate"
        );
        Ok(Self {
            generation,
            key,
            certificate: certificate.to_owned(),
        })
    }
    pub(crate) fn activate(&self, store: &mut impl Store, now: u64) -> Result<()> {
        let previous: Option<Generation> = load(store, SIGNING_GENERATION_KEY)?;
        let mut trusted = TrustStore::default();
        if let Some(generation) = previous {
            // An authority Durable Object owns one root identity for its lifetime;
            // TrustStore tracks rollback barriers per issuer, so cross-root swaps
            // must be rejected before overwriting the persisted generation.
            if generation.issuer != self.generation.issuer {
                tracing::error!(
                    event = "authority_signing_issuer_mismatch",
                    expected_issuer = %URL_SAFE_NO_PAD.encode(generation.issuer),
                    configured_issuer = %URL_SAFE_NO_PAD.encode(self.generation.issuer),
                    previous_generation = generation.number,
                    configured_generation = self.generation.number,
                );
                anyhow::bail!("signing root does not match persisted authority issuer");
            }
            trusted
                .remember(generation)
                .map_err(|error| anyhow!("{error:?}"))?;
        }
        trusted
            .accept(self.generation, now)
            .map_err(|error| anyhow!("{error:?}"))?;

        // Revalidating the active certificate does not advance the rollback barrier.
        if previous != Some(self.generation) {
            save(store, SIGNING_GENERATION_KEY, &self.generation)?;
            tracing::info!(
                event = "authority_signing_generation_activated",
                previous_generation = ?previous.map(|generation| generation.number),
                generation = self.generation.number,
            );
        }
        Ok(())
    }
}
