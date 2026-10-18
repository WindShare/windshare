use super::*;
use std::{
    collections::VecDeque,
    sync::{Mutex, atomic::AtomicBool},
};

enum Packet {
    Data(Vec<u8>, SocketAddr),
    Error(io::ErrorKind),
}

enum WriteResult {
    Success,
    Short,
    Error,
    Stall,
}

struct Scripted {
    packets: VecDeque<Packet>,
    writes: VecDeque<WriteResult>,
    responses: Arc<Mutex<Vec<Vec<u8>>>>,
    dropped: Arc<AtomicBool>,
}

impl Datagram for Scripted {
    fn receive(
        &mut self,
        buffer: &mut [u8],
    ) -> impl Future<Output = io::Result<(usize, SocketAddr)>> + Send {
        std::future::ready(
            match self
                .packets
                .pop_front()
                .expect("script includes terminal error")
            {
                Packet::Data(data, remote) => {
                    buffer[..data.len()].copy_from_slice(&data);
                    Ok((data.len(), remote))
                }
                Packet::Error(kind) => Err(kind.into()),
            },
        )
    }

    async fn send(&mut self, data: &[u8], _: SocketAddr) -> io::Result<usize> {
        match self.writes.pop_front().expect("write script") {
            WriteResult::Success => {
                self.responses.lock().unwrap().push(data.to_vec());
                Ok(data.len())
            }
            WriteResult::Short => Ok(0),
            WriteResult::Error => Err(io::ErrorKind::PermissionDenied.into()),
            WriteResult::Stall => std::future::pending().await,
        }
    }
}

impl Drop for Scripted {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::Release);
    }
}

fn binding() -> Vec<u8> {
    vec![
        0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12,
    ]
}

fn scripted(packets: Vec<Packet>, writes: Vec<WriteResult>) -> Scripted {
    Scripted {
        packets: packets.into(),
        writes: writes.into(),
        responses: Arc::default(),
        dropped: Arc::default(),
    }
}

#[tokio::test(start_paused = true)]
async fn packet_errors_write_failures_and_timeouts_do_not_retire_the_listener() {
    let remote = "127.0.0.1:1234".parse().unwrap();
    let mut packets = vec![
        Packet::Error(io::ErrorKind::ConnectionReset),
        Packet::Error(io::ErrorKind::ConnectionRefused),
        Packet::Error(io::ErrorKind::Interrupted),
        Packet::Data(
            vec![0; super::super::config::MAX_DATAGRAM_BYTES + 1],
            remote,
        ),
        Packet::Data(vec![0; 3], remote),
    ];
    packets.extend((0..4).map(|_| Packet::Data(binding(), remote)));
    packets.push(Packet::Error(io::ErrorKind::BrokenPipe));
    let socket = scripted(
        packets,
        vec![
            WriteResult::Short,
            WriteResult::Error,
            WriteResult::Stall,
            WriteResult::Success,
        ],
    );
    let responses = Arc::clone(&socket.responses);
    let dropped = Arc::clone(&socket.dropped);
    let status = Arc::new(ListenerStatus::new(0));
    let result = serve(socket, Limits::default(), Arc::clone(&status)).await;
    assert_eq!(result.unwrap_err().kind(), io::ErrorKind::BrokenPipe);
    assert!(!status.healthy());
    assert!(dropped.load(Ordering::Acquire));
    assert_eq!(responses.lock().unwrap().len(), 1);
    assert_eq!(status.metrics.received.load(Ordering::Relaxed), 6);
    assert_eq!(status.metrics.invalid.load(Ordering::Relaxed), 2);
    assert_eq!(status.metrics.write_errors.load(Ordering::Relaxed), 3);
    assert_eq!(status.metrics.read_errors.load(Ordering::Relaxed), 4);
    assert_eq!(status.metrics.responded.load(Ordering::Relaxed), 1);
}

#[tokio::test(start_paused = true)]
async fn malformed_packets_spend_budget_and_source_ports_cannot_bypass_it() {
    let a = "127.0.0.1:1".parse().unwrap();
    let b = "127.0.0.1:2".parse().unwrap();
    let socket = scripted(
        vec![
            Packet::Data(vec![0], a),
            Packet::Data(binding(), b),
            Packet::Error(io::ErrorKind::BrokenPipe),
        ],
        vec![],
    );
    let status = Arc::new(ListenerStatus::new(0));
    let limits = Limits {
        source_requests_per_second: 1.try_into().unwrap(),
        ..Limits::default()
    };
    assert!(serve(socket, limits, Arc::clone(&status)).await.is_err());
    assert_eq!(status.metrics.invalid.load(Ordering::Relaxed), 1);
    assert_eq!(status.metrics.limited.load(Ordering::Relaxed), 1);
}

#[tokio::test(start_paused = true)]
async fn aborting_a_blocked_write_destroys_its_socket_and_health_guard() {
    let socket = scripted(
        vec![Packet::Data(binding(), "127.0.0.1:1".parse().unwrap())],
        vec![WriteResult::Stall],
    );
    let dropped = Arc::clone(&socket.dropped);
    let status = Arc::new(ListenerStatus::new(0));
    let task = tokio::spawn(serve(socket, Limits::default(), Arc::clone(&status)));
    tokio::task::yield_now().await;
    assert!(status.healthy());
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    assert!(!status.healthy());
    assert!(dropped.load(Ordering::Acquire));
}

#[tokio::test]
async fn terminal_read_failure_retires_only_its_listener() {
    let healthy = Arc::new(ListenerStatus::new(1));
    let socket = bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let address = socket.local_addr().unwrap();
    let task = tokio::spawn(serve(socket, Limits::default(), Arc::clone(&healthy)));
    let failed = Arc::new(ListenerStatus::new(0));
    let broken = scripted(vec![Packet::Error(io::ErrorKind::BrokenPipe)], vec![]);
    assert!(
        serve(broken, Limits::default(), Arc::clone(&failed))
            .await
            .is_err()
    );
    let client = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    client.send_to(&binding(), address).await.unwrap();
    let mut response = [0; 100];
    timeout(Duration::from_secs(3), client.recv_from(&mut response))
        .await
        .unwrap()
        .unwrap();
    assert!(!failed.healthy());
    assert!(healthy.healthy());
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    assert!(!healthy.healthy());
}
