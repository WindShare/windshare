//! Gossip is a bounded control-plane service. Share actors never own HTTP probes.
mod book;
mod persistence;
mod record;
mod transport;

use anyhow::{Context, Result, ensure};
use book::{Book, SAMPLE};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{
    Method, Request, Response, StatusCode,
    body::{Bytes, Incoming},
    header,
};
use record::{Envelope, MAX_RECORD, Record};
use std::{
    net::IpAddr,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{sync::mpsc, task::JoinSet};
use windshare_relay_access::descriptor::Purpose;

pub const PATH: &str = "/discovery/v1/peers";
const LIVE: &str = "/.well-known/windshare";
const WORKERS: usize = 4;
const QUEUE: usize = 32;
const PERIOD: Duration = Duration::from_mins(10);
const MAX_SEEDS: usize = 16;
const EXCHANGE_FANOUT: usize = 2;

enum SelfGossip {
    LocalOnly,
    OptedOut,
    ObserverOnly,
    Advertise(Envelope),
}

fn self_gossip(node: &crate::node::Runtime, endpoint: &crate::Endpoint) -> SelfGossip {
    if transport::relay_origin(endpoint.relay_url()).is_err() {
        return SelfGossip::LocalOnly;
    }
    match node.gossip_descriptor(endpoint) {
        Ok(crate::node::GossipDescriptor::OptedOut) => SelfGossip::OptedOut,
        Ok(crate::node::GossipDescriptor::Advertise(envelope)) => {
            SelfGossip::Advertise(envelope.into())
        }
        Ok(crate::node::GossipDescriptor::ObserverOnly) | Err(_) => SelfGossip::ObserverOnly,
    }
}

struct InFlightGuard {
    book: Arc<Mutex<Book>>,
    key: String,
}

impl Drop for InFlightGuard {
    fn drop(&mut self) {
        if let Ok(mut book) = self.book.lock() {
            book.finish_probe(&self.key);
        }
    }
}

pub struct QueuedProbe {
    pub(super) record: Record,
    guard: InFlightGuard,
}

enum QueueOutcome {
    Queued,
    Skipped,
}

enum QueueError {
    Full,
}

#[derive(Clone)]
pub struct Discovery {
    book: Arc<Mutex<Book>>,
    sender: mpsc::Sender<QueuedProbe>,
    dirty: Arc<tokio::sync::Notify>,
}

impl Discovery {
    pub fn new() -> (Self, mpsc::Receiver<QueuedProbe>) {
        let (sender, receiver) = mpsc::channel(QUEUE);
        (
            Self {
                book: Arc::default(),
                dirty: Arc::default(),
                sender,
            },
            receiver,
        )
    }

    pub fn validate(seeds: &[String]) -> Result<()> {
        ensure!(seeds.len() <= MAX_SEEDS, "too many bootstrap nodes");
        for seed in seeds {
            transport::seed_origin(seed)?;
        }
        Ok(())
    }

    fn sample_peers(
        &self,
        now: u64,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
    ) -> Vec<Envelope> {
        // Reserve one slot for the local descriptor before sampling so the book cursor matches emitted peers.
        let own = match self_gossip(node, endpoint) {
            SelfGossip::Advertise(own) => Some(own),
            SelfGossip::LocalOnly | SelfGossip::OptedOut | SelfGossip::ObserverOnly => None,
        };
        let limit = SAMPLE - usize::from(own.is_some());
        let mut sample = self.book.lock().expect("peer book").sample(now, limit);
        if let Some(own) = own {
            sample.push(own);
        }
        sample
    }

    pub async fn handle(
        &self,
        request: Request<Incoming>,
        source: IpAddr,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
    ) -> Response<Full<Bytes>> {
        let now = crate::node::now().unsigned_abs();
        let (status, body) = match *request.method() {
            Method::GET => {
                let sample = self.sample_peers(now, node, endpoint);
                (
                    StatusCode::OK,
                    serde_json::to_vec(&sample).expect("signed records"),
                )
            }
            Method::POST => {
                if self
                    .book
                    .lock()
                    .expect("discovery ingress")
                    .allow(source, now)
                {
                    let parsed = async {
                        let body = Limited::new(request.into_body(), MAX_RECORD)
                            .collect()
                            .await
                            .map_err(|_| anyhow::anyhow!("request size"))?
                            .to_bytes();
                        let record = serde_json::from_slice::<Envelope>(&body)?
                            .verify(Purpose::Gossip, now)?;
                        ensure!(
                            record.offers_community_service(),
                            "peer does not offer community service"
                        );
                        Ok::<_, anyhow::Error>(record)
                    }
                    .await;
                    match parsed {
                        Ok(record) => {
                            match self.queue(record, source, node.public_key_b64(), endpoint, now) {
                                Ok(QueueOutcome::Queued | QueueOutcome::Skipped) => {
                                    (StatusCode::ACCEPTED, Vec::new())
                                }
                                Err(QueueError::Full) => {
                                    tracing::debug!(
                                        event = "discovery_rejected",
                                        %source,
                                        reason = "probe_queue_full"
                                    );
                                    (StatusCode::SERVICE_UNAVAILABLE, Vec::new())
                                }
                            }
                        }
                        Err(error) => {
                            tracing::debug!(event="discovery_rejected",%source,%error);
                            (StatusCode::BAD_REQUEST, Vec::new())
                        }
                    }
                } else {
                    tracing::debug!(event="discovery_rejected",%source,reason="rate_limit");
                    (StatusCode::TOO_MANY_REQUESTS, Vec::new())
                }
            }
            _ => (StatusCode::METHOD_NOT_ALLOWED, Vec::new()),
        };
        let mut builder = Response::builder()
            .status(status)
            .header(header::CONTENT_TYPE, "application/json")
            .header(header::CACHE_CONTROL, "no-store")
            .header(header::CONNECTION, "close");
        if status == StatusCode::SERVICE_UNAVAILABLE {
            builder = builder.header(header::RETRY_AFTER, crate::node::SERVICE_RETRY_SECONDS);
        }
        builder
            .body(Full::new(Bytes::from(body)))
            .expect("static response")
    }

    fn is_self(record: &Record, own_key: Option<&str>, endpoint: &crate::Endpoint) -> bool {
        record.endpoint == endpoint.as_str() || own_key.is_some_and(|key| key == record.key)
    }

    pub(super) fn prepare_restored_probe(
        &self,
        record: Record,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
        now: u64,
    ) -> Option<QueuedProbe> {
        if Self::is_self(&record, own_key, endpoint) {
            return None;
        }
        let mut book = self.book.lock().expect("peer book");
        if book
            .skip_probe_reason(&record.key, record.issued, now)
            .is_some()
        {
            return None;
        }
        let key = record.key.clone();
        book.begin_probe(key.clone());
        drop(book);
        Some(QueuedProbe {
            record,
            guard: InFlightGuard {
                book: Arc::clone(&self.book),
                key,
            },
        })
    }

    fn queue(
        &self,
        record: Record,
        source: impl std::fmt::Display,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
        now: u64,
    ) -> std::result::Result<QueueOutcome, QueueError> {
        if Self::is_self(&record, own_key, endpoint) {
            tracing::debug!(
                event = "discovery_probe_skipped",
                %source,
                reason = "self",
                node_key = %record.key,
                endpoint = %record.endpoint
            );
            return Ok(QueueOutcome::Skipped);
        }
        // Check known/in-flight and reserve a probe slot atomically so duplicate
        // replays never evict newcomers from the bounded queue or race into two probes.
        let mut book = self.book.lock().expect("peer book");
        if let Some(reason) = book.skip_probe_reason(&record.key, record.issued, now) {
            drop(book);
            tracing::debug!(
                event = "discovery_probe_skipped",
                %source,
                reason,
                node_key = %record.key,
                endpoint = %record.endpoint
            );
            return Ok(QueueOutcome::Skipped);
        }
        let Ok(permit) = self.sender.try_reserve() else {
            return Err(QueueError::Full);
        };
        let key = record.key.clone();
        book.begin_probe(key.clone());
        drop(book);
        tracing::debug!(event="discovery_probe_queued",%source,node_key=%record.key,endpoint=%record.endpoint);
        permit.send(QueuedProbe {
            record,
            guard: InFlightGuard {
                book: Arc::clone(&self.book),
                key,
            },
        });
        Ok(QueueOutcome::Queued)
    }

    async fn verify(&self, probe: QueuedProbe) -> Result<()> {
        let QueuedProbe { record, guard } = probe;
        let target = record.origin.join(LIVE)?;
        let client = transport::pinned(&target).await?;
        let response = client.get(target).send().await?;
        let ip = transport::verified_remote_ip(&response)?;
        let data = transport::body(response, MAX_RECORD).await?;
        let now = crate::node::now().unsigned_abs();
        let live = serde_json::from_slice::<Envelope>(&data)?.verify(Purpose::Live, now)?;
        ensure!(
            record.key == live.key
                && record.endpoint == live.endpoint
                && record.public_relay == live.public_relay
                && record.services == live.services
                && record.authority == live.authority
                && live.offers_community_service(),
            "live descriptor mismatch"
        );
        let key = record.key.clone();
        self.book
            .lock()
            .expect("peer book")
            .insert(record, ip, now)?;
        // Release the in-flight marker only after the verified record is visible in Book.
        drop(guard);
        self.dirty.notify_one();
        tracing::debug!(event="discovery_peer_verified",node_key=%key,%ip);
        Ok(())
    }

    async fn exchange_with_client(
        &self,
        client: &reqwest::Client,
        url: &url::Url,
        peer: &str,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
        own: Option<&Envelope>,
    ) -> Result<()> {
        // Pull before announcing so remote POST rejections (rate limits or a full
        // probe queue) never block peer sampling or mark a busy peer dead, while
        // unreachable peers still fail fast on a single request timeout.
        let bytes =
            transport::body(client.get(url.clone()).send().await?, SAMPLE * MAX_RECORD).await?;
        let entries: Vec<Envelope> = serde_json::from_slice(&bytes)?;
        ensure!(entries.len() <= SAMPLE, "oversized sample");
        let now = crate::node::now().unsigned_abs();
        for entry in entries {
            let verified = entry.verify(Purpose::Gossip, now).and_then(|record| {
                ensure!(
                    record.offers_community_service(),
                    "peer does not offer community service"
                );
                Ok(record)
            });
            match verified {
                Ok(record) => {
                    if self.queue(record, peer, own_key, endpoint, now).is_err() {
                        tracing::debug!(
                            event = "discovery_rejected",
                            source = %peer,
                            reason = "probe_queue_full"
                        );
                    }
                }
                Err(error) => {
                    tracing::debug!(event="discovery_rejected", source=%peer, %error);
                }
            }
        }
        if let Some(own) = own {
            match client.post(url.clone()).json(own).send().await {
                Ok(response) => {
                    if let Err(error) = response.error_for_status() {
                        tracing::debug!(event = "discovery_announce_rejected", %peer, %error);
                    }
                }
                Err(error) => {
                    tracing::debug!(event = "discovery_announce_failed", %peer, %error);
                }
            }
        }
        Ok(())
    }

    async fn exchange(
        &self,
        peer: &str,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
        own: Option<&Envelope>,
    ) -> Result<()> {
        let url = transport::relay_origin(peer)?.join(PATH)?;
        let client = transport::pinned(&url).await?;
        self.exchange_with_client(&client, &url, peer, own_key, endpoint, own)
            .await
    }

    fn exchange_targets(
        &self,
        now: u64,
        endpoint: &crate::Endpoint,
        seeds: &[String],
    ) -> Vec<String> {
        let own_origin = transport::canonical_origin_str(endpoint.relay_url()).ok();
        let mut targets = self.book.lock().expect("peer book").targets(now);
        targets.extend(
            seeds
                .iter()
                .filter_map(|seed| transport::canonical_origin_str(seed).ok()),
        );
        if let Some(own) = own_origin.as_deref() {
            targets.retain(|target| target != own);
        }
        targets.sort_unstable();
        targets.dedup();
        targets
    }

    async fn exchange_round(
        &self,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
        seeds: &[String],
        own: Option<&Envelope>,
        cursor: usize,
    ) {
        let targets = self.exchange_targets(crate::node::now().unsigned_abs(), endpoint, seeds);
        if targets.is_empty() {
            return;
        }
        for peer in targets
            .iter()
            .cycle()
            .skip(cursor % targets.len())
            .take(targets.len().min(EXCHANGE_FANOUT))
        {
            match self.exchange(peer, own_key, endpoint, own).await {
                Ok(()) => {
                    self.book
                        .lock()
                        .expect("peer book")
                        .note_exchange_success(peer);
                }
                Err(error) => {
                    tracing::debug!(event = "discovery_exchange_failed", %peer, %error);
                    let evicted = self
                        .book
                        .lock()
                        .expect("peer book")
                        .note_exchange_failure(peer);
                    if evicted {
                        self.dirty.notify_one();
                    }
                }
            }
        }
    }

    async fn announce(&self, node: &crate::node::Runtime, endpoint: &crate::Endpoint) {
        if transport::relay_origin(endpoint.relay_url()).is_err() {
            // Non-public or cleartext nodes stay out of public gossip entirely;
            // keep the task alive because `Server::serve` treats discovery exit as fatal.
            std::future::pending::<()>().await;
        }
        let mut seeds_watch = node.subscribe_bootstrap_nodes();
        let mut cursor = 0usize;
        loop {
            let seeds = Arc::clone(&*seeds_watch.borrow_and_update());
            match self_gossip(node, endpoint) {
                SelfGossip::LocalOnly | SelfGossip::OptedOut => {}
                SelfGossip::ObserverOnly => {
                    self.exchange_round(node.public_key_b64(), endpoint, &seeds, None, cursor)
                        .await;
                    cursor = cursor.wrapping_add(EXCHANGE_FANOUT);
                }
                SelfGossip::Advertise(own) => {
                    self.exchange_round(
                        node.public_key_b64(),
                        endpoint,
                        &seeds,
                        Some(&own),
                        cursor,
                    )
                    .await;
                    cursor = cursor.wrapping_add(EXCHANGE_FANOUT);
                }
            }
            tokio::select! {
                () = tokio::time::sleep(PERIOD) => {}
                changed = seeds_watch.changed() => {
                    if changed.is_ok() {
                        let count = seeds_watch.borrow().len();
                        tracing::debug!(event = "discovery_bootstrap_nodes_reloaded", count);
                    } else {
                        tokio::time::sleep(PERIOD).await;
                    }
                }
            }
        }
    }

    pub async fn run(
        &self,
        mut receiver: mpsc::Receiver<QueuedProbe>,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
        store: &crate::Database,
    ) {
        let persistence = self.maintain_cache(store, node, endpoint);
        tokio::pin!(persistence);
        let announce = self.announce(node, endpoint);
        tokio::pin!(announce);
        let mut workers = JoinSet::new();
        loop {
            tokio::select! {
                ()=&mut announce=>return,
                ()=&mut persistence=>return,
                probe=receiver.recv(),if workers.len()<WORKERS=>{
                    let Some(probe)=probe else{return;};
                    if Self::is_self(&probe.record, node.public_key_b64(), endpoint) {continue;}
                    let this=self.clone();
                    workers.spawn(async move {
                        let address=probe.record.endpoint.clone();
                        if let Err(error)=this.verify(probe).await.with_context(||"live probe") {
                            tracing::debug!(event="discovery_rejected",endpoint=%address,%error);
                        }
                    });
                }
                _=workers.join_next(),if !workers.is_empty()=>{}
            }
        }
    }
}

#[cfg(test)]
#[path = "discovery_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "ingress_tests.rs"]
mod ingress_tests;
