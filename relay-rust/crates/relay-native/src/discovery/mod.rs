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
    if transport::origin(endpoint.relay_url()).is_err() {
        return SelfGossip::LocalOnly;
    }
    match node.gossip_descriptor(endpoint) {
        Ok(crate::node::GossipDescriptor::OptedOut) => SelfGossip::OptedOut,
        Ok(crate::node::GossipDescriptor::Advertise(value)) => {
            serde_json::from_value(value).map_or(SelfGossip::ObserverOnly, SelfGossip::Advertise)
        }
        Ok(crate::node::GossipDescriptor::ObserverOnly) | Err(_) => SelfGossip::ObserverOnly,
    }
}

#[derive(Clone)]
pub struct Discovery {
    book: Arc<Mutex<Book>>,
    sender: mpsc::Sender<Record>,
    dirty: Arc<tokio::sync::Notify>,
}

impl Discovery {
    pub fn new() -> (Self, mpsc::Receiver<Record>) {
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
                    let result = async {
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
                        self.queue(record, source)
                    }
                    .await;
                    match result {
                        Ok(()) => (StatusCode::ACCEPTED, Vec::new()),
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
        Response::builder()
            .status(status)
            .header("content-type", "application/json")
            .header("connection", "close")
            .body(Full::new(Bytes::from(body)))
            .expect("static response")
    }

    fn queue(&self, record: Record, source: impl std::fmt::Display) -> Result<()> {
        let permit = self
            .sender
            .try_reserve()
            .map_err(|_| anyhow::anyhow!("probe queue full"))?;
        tracing::debug!(event="discovery_probe_queued",%source,node_key=%record.key,endpoint=%record.endpoint);
        permit.send(record);
        Ok(())
    }

    async fn verify(&self, record: Record) -> Result<()> {
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
        self.dirty.notify_one();
        tracing::debug!(event="discovery_peer_verified",node_key=%key,%ip);
        Ok(())
    }

    async fn exchange_with_client(
        &self,
        client: &reqwest::Client,
        url: &url::Url,
        peer: &str,
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
            let result = entry.verify(Purpose::Gossip, now).and_then(|record| {
                ensure!(
                    record.offers_community_service(),
                    "peer does not offer community service"
                );
                self.queue(record, peer)
            });
            if let Err(error) = result {
                tracing::debug!(event="discovery_rejected", source=%peer, %error);
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

    async fn exchange(&self, peer: &str, own: Option<&Envelope>) -> Result<()> {
        let url = transport::origin(peer)?.join(PATH)?;
        let client = transport::pinned(&url).await?;
        self.exchange_with_client(&client, &url, peer, own).await
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
            match self.exchange(peer, own).await {
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

    async fn announce(
        &self,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
        seeds: &[String],
    ) {
        if transport::origin(endpoint.relay_url()).is_err() {
            // Non-public or cleartext nodes stay out of public gossip entirely;
            // keep the task alive because `Server::serve` treats discovery exit as fatal.
            std::future::pending::<()>().await;
        }
        let mut cursor = 0usize;
        loop {
            match self_gossip(node, endpoint) {
                SelfGossip::LocalOnly | SelfGossip::OptedOut => {}
                SelfGossip::ObserverOnly => {
                    self.exchange_round(endpoint, seeds, None, cursor).await;
                    cursor = cursor.wrapping_add(EXCHANGE_FANOUT);
                }
                SelfGossip::Advertise(own) => {
                    self.exchange_round(endpoint, seeds, Some(&own), cursor)
                        .await;
                    cursor = cursor.wrapping_add(EXCHANGE_FANOUT);
                }
            }
            tokio::time::sleep(PERIOD).await;
        }
    }

    pub async fn run(
        &self,
        mut receiver: mpsc::Receiver<Record>,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
        seeds: &[String],
        store: &crate::Database,
    ) {
        let persistence = self.maintain_cache(store);
        tokio::pin!(persistence);
        let announce = self.announce(node, endpoint, seeds);
        tokio::pin!(announce);
        let mut workers = JoinSet::new();
        loop {
            tokio::select! {
                ()=&mut announce=>return,
                ()=&mut persistence=>return,
                record=receiver.recv(),if workers.len()<WORKERS=>{
                    let Some(record)=record else{return;};
                    if record.endpoint==endpoint.as_str() {continue;}
                    let this=self.clone();
                    workers.spawn(async move {
                        let address=record.endpoint.clone();
                        if let Err(error)=this.verify(record).await.with_context(||"live probe") {
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
