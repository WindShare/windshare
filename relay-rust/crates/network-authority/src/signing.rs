use crate::store::{Store, load, save};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::trust::{Generation, TrustStore};

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
        let mut trusted = TrustStore::default();
        if let Some(generation) = load(store, "signing-generation")? {
            trusted
                .remember(generation)
                .map_err(|error| anyhow!("{error:?}"))?;
        }
        trusted
            .accept(self.generation, now)
            .map_err(|error| anyhow!("{error:?}"))?;
        save(store, "signing-generation", &self.generation)
    }
}
