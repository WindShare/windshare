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
            transport::origin(seed)?;
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
        let own = node
            .sign_descriptor(endpoint, Purpose::Gossip)
            .and_then(|v| Ok(serde_json::from_value(v)?))
            .ok();
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
        let (client, ip) = transport::pinned(&target).await?;
        let data = transport::body(client.get(target).send().await?, MAX_RECORD).await?;
        let now = crate::node::now().unsigned_abs();
        let live = serde_json::from_slice::<Envelope>(&data)?.verify(Purpose::Live, now)?;
        ensure!(
            record.key == live.key
                && record.endpoint == live.endpoint
                && record.public_relay == live.public_relay,
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

    async fn exchange(&self, peer: &str, own: &serde_json::Value) -> Result<()> {
        let url = transport::origin(peer)?.join(PATH)?;
        let (client, _) = transport::pinned(&url).await?;
        client
            .post(url.clone())
            .json(own)
            .send()
            .await?
            .error_for_status()?;
        let bytes = transport::body(client.get(url).send().await?, SAMPLE * MAX_RECORD).await?;
        let entries: Vec<Envelope> = serde_json::from_slice(&bytes)?;
        ensure!(entries.len() <= SAMPLE, "oversized sample");
        let now = crate::node::now().unsigned_abs();
        for entry in entries {
            if let Err(error) = entry
                .verify(Purpose::Gossip, now)
                .and_then(|record| self.queue(record, peer))
            {
                tracing::debug!(event="discovery_rejected", source=%peer, %error);
            }
        }
        Ok(())
    }

    async fn announce(
        &self,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
        seeds: &[String],
    ) {
        let mut cursor = 0usize;
        loop {
            let mut targets = self
                .book
                .lock()
                .expect("peer book")
                .targets(crate::node::now().unsigned_abs());
            targets.extend_from_slice(seeds);
            targets.sort();
            targets.dedup();
            if let Ok(own) = node.sign_descriptor(endpoint, Purpose::Gossip) {
                for peer in targets
                    .iter()
                    .cycle()
                    .skip(cursor % targets.len().max(1))
                    .take(targets.len().min(2))
                {
                    if let Err(error) = self.exchange(peer, &own).await {
                        tracing::debug!(event="discovery_exchange_failed",%peer,%error);
                    }
                }
            }
            cursor = cursor.wrapping_add(2);
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
