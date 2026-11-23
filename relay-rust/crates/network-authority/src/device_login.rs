use crate::{
    Account, Authority, Environment, Fault, Node, PasswordVerifier, Store, StoredPassword,
    store::{key, load},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use windshare_relay_access::{
    device_login::{Approval, Invitation},
    trust,
};

const MAX_SESSION_LENGTH: usize = 1024;
const SESSION_SECONDS: u64 = 12 * 3600;
const SESSION_DOMAIN: &[u8] = b"windshare/service-browser-session/v1\0";

#[derive(Serialize, Deserialize)]
struct BrowserSession {
    account: [u8; 16],
    expires: u64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct DeviceApproval {
    pub origin: String,
    pub approval: String,
    pub session: String,
}

fn session_domain(password: &StoredPassword) -> Vec<u8> {
    // A password change invalidates browser consent without publishing a password verifier in the cookie.
    [SESSION_DOMAIN, password.fingerprint().as_slice()].concat()
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Only enrolled and enabled nodes may direct an account holder to a return origin.
    pub fn device_invitation(&self, token: &str) -> Result<Invitation> {
        let invite = Invitation::verify(
            token,
            self.signing.generation.issuer,
            self.environment.now() * 1000,
        )
        .map_err(|_| Fault::Denied {
            reason: "invalid_invitation".into(),
        })?;
        let node = load::<Node>(&self.store, &key("node", &invite.node))?;
        ensure!(
            node.is_some_and(
                |n| n.admission() == windshare_relay_access::usage::Admission::Accepting
            ),
            Fault::Denied {
                reason: "node_unavailable".into()
            }
        );
        let origin = url::Url::parse(&invite.origin).map_err(|_| Fault::Invalid)?;
        let loopback = match origin.host() {
            Some(url::Host::Ipv4(ip)) => ip.is_loopback(),
            Some(url::Host::Ipv6(ip)) => ip.is_loopback(),
            Some(url::Host::Domain("localhost")) => true,
            _ => false,
        };
        ensure!(
            origin.origin().ascii_serialization() == invite.origin
                && (origin.scheme() == "https" || origin.scheme() == "http" && loopback),
            Fault::Invalid
        );
        Ok(invite)
    }

    pub(crate) fn browser_account(&self, cookie: &str) -> Option<Account> {
        if cookie.len() > MAX_SESSION_LENGTH {
            return None;
        }
        let signed = URL_SAFE_NO_PAD.decode(cookie).ok()?;
        let (payload, _) = signed.split_last_chunk::<64>()?;
        let session: BrowserSession = serde_json::from_slice(payload).ok()?;
        let account = self.ensure_account(session.account).ok()?;
        trust::verify(
            &session_domain(account.password.as_ref()?),
            &signed,
            &self.signing.generation.key,
        )
        .ok()?;
        (session.expires > self.environment.now()).then_some(account)
    }

    /// Validates the device invitation and account password verifier atomically,
    /// then issues both the browser consent cookie and one-time device receipt.
    ///
    /// # Errors
    /// Refuses invalid invitations, disabled nodes, rate-limited callers, wrong credentials, or blocked accounts.
    pub fn device_sign_in(
        &mut self,
        source: &str,
        invitation: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<DeviceApproval> {
        let invite = self.device_invitation(invitation)?;
        let account = self.verify_account_login(source, name, verifier)?;
        let session = self.sign_browser_session(&account)?;
        let approval = self.approve_device(&invite, &account)?;
        Ok(DeviceApproval {
            origin: invite.origin,
            approval,
            session,
        })
    }

    fn sign_browser_session(&self, account: &Account) -> Result<String> {
        let session = BrowserSession {
            account: account.id,
            expires: (self.environment.now() + SESSION_SECONDS)
                .min(self.signing.generation.expires_at),
        };
        Ok(URL_SAFE_NO_PAD.encode(trust::sign(
            &session_domain(account.password.as_ref().ok_or(Fault::Invalid)?),
            &serde_json::to_vec(&session)?,
            &self.signing.key,
        )))
    }

    /// # Errors
    /// The receipt is usable for one enrollment and contains no password or reusable service ticket.
    pub fn approve_device(&self, invite: &Invitation, account: &Account) -> Result<String> {
        tracing::info!(event="service_device_approved",node=?invite.node,enrollment=?invite.enrollment,account=?account.id);
        Approval {
            node: invite.node,
            issuer: invite.issuer,
            enrollment: invite.enrollment,
            account: account.id,
            entitlement: self.entitlement(account.id)?,
            expires_ms: invite.expires_ms,
        }
        .issue(&self.signing.key)
        .map_err(|_| Fault::Invalid.into())
    }
}
