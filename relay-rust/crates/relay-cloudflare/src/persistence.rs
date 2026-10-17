use crate::config::Route;
use anyhow::{Result, anyhow, ensure};
use serde::{Deserialize, Serialize};
use std::num::NonZeroU64;
use windshare_relay_core::{
    authority::StoredAuthority,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::{ConnectionRef, Incarnation},
    share::Identity,
};

// This version has never admitted JOIN or opaque frames. Recovery may prove zero
// delivery responsibility from that fact, never merely from disappearing sockets.
const LIFECYCLE_SCHEMA: u32 = 1;

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Image {
    format: u32,
    endpoint: String,
    share: [u8; 12],
    relay: [u8; 32],
    last_connection: u64,
}

impl Image {
    #[must_use]
    pub fn new(route: &Route) -> Self {
        Self {
            format: LIFECYCLE_SCHEMA,
            endpoint: route.endpoint.clone(),
            share: *route.share.as_bytes(),
            relay: *route.relay.as_bytes(),
            last_connection: 0,
        }
    }

    /// # Errors
    /// Refuses a different public endpoint or route in the same Durable Object.
    pub fn check_route(&self, route: &Route) -> Result<()> {
        ensure!(
            self.format == LIFECYCLE_SCHEMA,
            "unsupported Cloudflare storage profile"
        );
        ensure!(
            self.endpoint == route.endpoint
                && self.share == *route.share.as_bytes()
                && self.relay == *route.relay.as_bytes(),
            "Durable Object identity mismatch"
        );
        Ok(())
    }

    /// # Errors
    /// Invalid or unknown storage is never treated as an empty share.
    pub fn identity(&self, incarnation: Incarnation) -> Result<Identity> {
        ensure!(
            self.format == LIFECYCLE_SCHEMA,
            "unsupported Cloudflare storage profile"
        );
        let route = Route::parse(&format!(
            "{}?share_id={}",
            self.endpoint
                .replacen("wss:", "https:", 1)
                .replacen("ws:", "http:", 1),
            base64::Engine::encode(
                &base64::engine::general_purpose::URL_SAFE_NO_PAD,
                self.share
            )
        ))?;
        self.check_route(&route)?;
        Ok(Identity {
            relay: route.relay,
            share: route.share,
            incarnation,
        })
    }

    /// The caller commits the updated image before accepting the socket.
    ///
    /// # Errors
    /// Connection lifetimes are never recycled, even after a restart.
    pub fn allocate(&mut self) -> Result<ConnectionRef> {
        self.last_connection = self
            .last_connection
            .checked_add(1)
            .ok_or_else(|| anyhow!("connection identity exhausted"))?;
        Ok(ConnectionRef {
            id: NonZeroU64::new(self.last_connection).expect("checked positive sequence"),
            generation: NonZeroU64::MIN,
        })
    }
}

#[derive(Serialize, Deserialize)]
pub struct AuthorityRow {
    record: Vec<u8>,
    descriptor: Option<Vec<u8>>,
    publication: Option<Vec<u8>>,
}

impl AuthorityRow {
    pub fn new(stored: &StoredAuthority) -> Self {
        Self {
            record: stored.record().encode().as_bytes().to_vec(),
            descriptor: stored.descriptor().map(<[u8]>::to_vec),
            publication: stored.publication_key().map(<[u8]>::to_vec),
        }
    }

    pub fn decode(self, relay: RelayIdentity, share: ShareId) -> Result<StoredAuthority> {
        StoredAuthority::decode(
            relay,
            share,
            &self.record,
            self.descriptor,
            self.publication,
        )
        .map_err(|error| anyhow!("invalid authority row: {error:?}"))
    }
}

pub fn tag(connection: ConnectionRef) -> String {
    format!("c-{}-{}", connection.id, connection.generation)
}

pub fn parse_tag(tag: &str) -> Option<ConnectionRef> {
    let mut fields = tag.strip_prefix("c-")?.split('-');
    let id = NonZeroU64::new(fields.next()?.parse().ok()?)?;
    let generation = NonZeroU64::new(fields.next()?.parse().ok()?)?;
    (fields.next().is_none()).then_some(ConnectionRef { id, generation })
}
