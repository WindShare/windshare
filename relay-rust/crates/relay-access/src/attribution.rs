use serde::{Deserialize, Serialize};

pub type Issuer = [u8; 32];
pub type AccountId = [u8; 16];
pub type CredentialId = [u8; 16];

/// The issuer namespace survives online key rotation; credentials never own fairness.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
pub enum Principal {
    Local([u8; 16]),
    Account { issuer: Issuer, account: AccountId },
    Anonymous([u8; 12]),
}

/// Stable service ownership, independent of the sender's cryptographic share identity.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum Attribution {
    Anonymous,
    Credential {
        principal: [u8; 16],
        credential: CredentialId,
    },
    Account {
        issuer: Issuer,
        account: AccountId,
        credential: CredentialId,
    },
}

impl Attribution {
    pub const ENCODED_BYTES: usize = 65;

    pub const fn principal(self, anonymous_share: [u8; 12]) -> Principal {
        match self {
            Self::Anonymous => Principal::Anonymous(anonymous_share),
            Self::Credential { principal, .. } => Principal::Local(principal),
            Self::Account {
                issuer, account, ..
            } => Principal::Account { issuer, account },
        }
    }

    pub const fn credential(self) -> Option<CredentialId> {
        match self {
            Self::Anonymous => None,
            Self::Credential { credential, .. } | Self::Account { credential, .. } => {
                Some(credential)
            }
        }
    }

    pub fn encode(self) -> [u8; Self::ENCODED_BYTES] {
        let mut bytes = [0; Self::ENCODED_BYTES];
        match self {
            Self::Anonymous => {}
            Self::Credential {
                principal,
                credential,
            } => {
                bytes[0] = 1;
                bytes[1..17].copy_from_slice(&principal);
                bytes[17..33].copy_from_slice(&credential);
            }
            Self::Account {
                issuer,
                account,
                credential,
            } => {
                bytes[0] = 2;
                bytes[1..33].copy_from_slice(&issuer);
                bytes[33..49].copy_from_slice(&account);
                bytes[49..65].copy_from_slice(&credential);
            }
        }
        bytes
    }

    pub fn decode(bytes: [u8; Self::ENCODED_BYTES]) -> Option<Self> {
        match bytes[0] {
            0 if bytes[1..].iter().all(|byte| *byte == 0) => Some(Self::Anonymous),
            1 if bytes[33..].iter().all(|byte| *byte == 0) => {
                let principal = bytes[1..17].try_into().ok()?;
                let credential = bytes[17..33].try_into().ok()?;
                (principal != [0; 16] && credential != [0; 16]).then_some(Self::Credential {
                    principal,
                    credential,
                })
            }
            2 => {
                let issuer = bytes[1..33].try_into().ok()?;
                let account = bytes[33..49].try_into().ok()?;
                let credential = bytes[49..65].try_into().ok()?;
                (issuer != [0; 32] && account != [0; 16] && credential != [0; 16]).then_some(
                    Self::Account {
                        issuer,
                        account,
                        credential,
                    },
                )
            }
            _ => None,
        }
    }
}
