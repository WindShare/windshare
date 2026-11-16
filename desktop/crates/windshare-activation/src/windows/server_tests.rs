use super::*;
use crate::{IntentId, Phase};

const RECOVERY_TIMEOUT: Duration = Duration::from_secs(2);
const RECONNECT_INTERVAL: Duration = Duration::from_millis(5);

#[test]
fn listener_recovers_when_client_disconnects_before_accept() {
    let scope = Scope::named(&uuid::Uuid::new_v4().to_string()).unwrap();
    let pipe = Pipe::listen(&scope.pipe, &scope.sid).unwrap();
    // Seed the failed instance before the worker runs, without a scheduling race.
    drop(Pipe::connect(&scope.pipe).unwrap());
    let closing = Arc::new(AtomicBool::new(false));
    let stop = closing.clone();
    let listener_scope = scope.clone();
    let instance = IntentId::new();
    let worker = std::thread::spawn(move || {
        run(&listener_scope, pipe, &stop, &|request| {
            assert!(matches!(request, Request::Probe));
            Reply::Status {
                instance,
                phase: Phase::Running,
            }
        });
    });
    let _server = Server {
        closing,
        worker: Some(worker),
    };
    let deadline = Instant::now() + RECOVERY_TIMEOUT;
    let reply = loop {
        match exchange(&scope, &Request::Probe, deadline) {
            Ok(reply) => break reply,
            Err(ExchangeFailure::NotConnected(_)) if Instant::now() < deadline => {
                std::thread::sleep(RECONNECT_INTERVAL);
            }
            Err(error) => panic!("listener did not recover: {error:?}"),
        }
    };
    assert!(
        matches!(reply, Reply::Status { instance: id, phase: Phase::Running } if id == instance)
    );
}
