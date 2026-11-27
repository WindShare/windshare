use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};
use std::{fs::OpenOptions, io::Write, path::Path};
use windshare_relay_access::{Credential, CredentialState, Digest};

const NODE_ID_DOMAIN: &[u8] = b"windshare/node-identity/v1\0";
const IDENTITY_FORMAT: u8 = 1;

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Identity {
    format: u8,
    seed: [u8; 32],
    owner_token: String,
    owner_principal: [u8; 16],
    owner_credential: [u8; 16],
}

impl Identity {
    /// # Errors
    /// Fails when the operating system cannot supply secure entropy.
    pub fn generate() -> Result<Self> {
        Ok(Self {
            format: IDENTITY_FORMAT,
            seed: random()?,
            owner_token: format!("wsak_{}", URL_SAFE_NO_PAD.encode(random::<32>()?)),
            owner_principal: random()?,
            owner_credential: random()?,
        })
    }

    /// # Errors
    /// Fails on invalid identity data or unavailable persistent storage.
    pub fn open(path: &Path) -> Result<Self> {
        Self::open_for_endpoint(path, false)
    }

    /// # Errors
    /// Fails when the persisted identity cannot be read or created.
    pub fn open_for_endpoint(path: &Path, https: bool) -> Result<Self> {
        match std::fs::read(path) {
            Ok(bytes) => {
                let identity: Self = serde_json::from_slice(&bytes)?;
                ensure!(
                    identity.format == IDENTITY_FORMAT
                        && identity.seed != [0; 32]
                        && identity.owner_principal != [0; 16]
                        && identity.owner_credential != [0; 16]
                        && identity
                            .owner_token
                            .strip_prefix("wsak_")
                            .and_then(|token| URL_SAFE_NO_PAD.decode(token).ok())
                            .is_some_and(|bytes| bytes.len() == 32),
                    "invalid node identity"
                );
                Ok(identity)
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                let identity = Self::generate()?;
                let mut options = OpenOptions::new();
                options.write(true).create_new(true);
                #[cfg(unix)]
                {
                    use std::os::unix::fs::OpenOptionsExt;
                    options.mode(0o600);
                }
                // Publish only a complete, durable identity. A concurrent first
                // start must adopt the winner instead of replacing its node key.
                let temporary = TemporaryIdentity(path.with_extension(format!(
                    "identity-{}.tmp",
                    URL_SAFE_NO_PAD.encode(random::<16>()?)
                )));
                let mut file = options.open(&temporary.0)?;
                file.write_all(&serde_json::to_vec(&identity)?)?;
                file.sync_all()?;
                drop(file);
                match std::fs::hard_link(&temporary.0, path) {
                    Ok(()) => {}
                    Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                        return Self::open_for_endpoint(path, https);
                    }
                    Err(error) => return Err(error).context("publish node identity"),
                }
                #[cfg(unix)]
                std::fs::File::open(
                    path.parent()
                        .filter(|parent| !parent.as_os_str().is_empty())
                        .unwrap_or_else(|| Path::new(".")),
                )?
                .sync_all()?;
                let mut event = serde_json::json!({"event":"node_initialized","node_id":identity.id(),"node_key":URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes()),"identity_file":path});
                if !https {
                    event["owner_access_key"] =
                        serde_json::Value::String(identity.owner_token.clone());
                }
                println!("{event}");
                Ok(identity)
            }
            Err(error) => Err(error).context("read node identity"),
        }
    }

    pub fn key(&self) -> SigningKey {
        SigningKey::from_bytes(&self.seed)
    }

    pub(crate) fn initialization_token(&self) -> String {
        use ed25519_dalek::Signer;
        // A restart can reprint the pending link without storing another plaintext secret.
        URL_SAFE_NO_PAD.encode(Sha256::digest(
            self.key()
                .sign(b"windshare/node-console/initialize/v1")
                .to_bytes(),
        ))
    }

    pub fn id(&self) -> String {
        node_id(&self.key().verifying_key().to_bytes())
    }

    pub fn owner(&self) -> Credential {
        Credential {
            principal: self.owner_principal,
            id: self.owner_credential,
            name: "Node owner".into(),
            digest: Digest::of(self.owner_token.as_bytes()),
            created_at: 0,
            last_used_at: None,
            state: CredentialState::Active,
        }
    }
}

struct TemporaryIdentity(std::path::PathBuf);
impl Drop for TemporaryIdentity {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

#[cfg(test)]
#[path = "identity_tests.rs"]
mod tests;

pub(super) fn node_id(verifying_key: &[u8; 32]) -> String {
    URL_SAFE_NO_PAD.encode(
        Sha256::new()
            .chain_update(NODE_ID_DOMAIN)
            .chain_update(verifying_key)
            .finalize(),
    )
}

pub(super) fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|error| anyhow::anyhow!("node entropy: {error}"))?;
    Ok(bytes)
}
