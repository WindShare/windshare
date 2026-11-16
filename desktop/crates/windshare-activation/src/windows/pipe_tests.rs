use super::*;
use crate::{Activation, IntentId, Phase, Reply, Request};
use std::{
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

fn isolated_scope() -> Scope {
    Scope::named(&uuid::Uuid::new_v4().to_string()).unwrap()
}

#[test]
fn instance_election_is_exclusive_until_owner_drops() {
    let scope = isolated_scope();
    let owner = scope.elect().unwrap().unwrap();
    let contender = scope.clone();
    assert!(
        !std::thread::spawn(move || contender.elect().unwrap().is_some())
            .join()
            .unwrap()
    );
    drop(owner);
    assert!(
        std::thread::spawn(move || scope.elect().unwrap().is_some())
            .join()
            .unwrap()
    );
}

#[test]
fn pipe_exchanges_bound_session_identity_and_preserve_retried_intent() {
    let scope = isolated_scope();
    let _owner = scope.elect().unwrap().unwrap();
    let instance = IntentId::new();
    let received = Arc::new(Mutex::new(Vec::new()));
    let capture = received.clone();
    let _server = Server::start(scope.clone(), move |request| match request {
        Request::Probe => Reply::Status {
            instance,
            phase: Phase::Running,
        },
        Request::Activate {
            instance: target,
            activation,
        } => {
            assert_eq!(target, instance);
            capture.lock().unwrap().push(activation);
            Reply::Accepted { instance }
        }
    })
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(3);
    assert!(
        matches!(server::exchange(&scope, &Request::Probe, deadline).unwrap(), Reply::Status { instance: id, .. } if id == instance)
    );
    // Reconnection may land in the brief listener replacement interval.
    let activation = Activation::ShareSelection(crate::ShareIntent::new(
        crate::EntrySource::Explorer,
        vec![std::env::current_dir().unwrap().join("原生.txt")],
    ));
    for _ in 0..2 {
        let scope = scope.clone();
        let activation = activation.clone();
        std::thread::spawn(move || {
            dispatch::deliver(&scope, &activation, std::path::Path::new("must-not-launch"))
        })
        .join()
        .unwrap()
        .unwrap();
    }
    assert_eq!(
        *received.lock().unwrap(),
        vec![activation.clone(), activation]
    );
}

#[test]
fn accept_timeouts_preserve_the_listener_for_a_later_client() {
    let scope = isolated_scope();
    let server = pipe::Pipe::listen(&scope.pipe, &scope.sid).unwrap();
    for _ in 0..3 {
        let error = server.accept(Instant::now()).unwrap_err();
        assert_eq!(
            error.code(),
            windows::Win32::Foundation::ERROR_TIMEOUT.to_hresult()
        );
    }
    let client = pipe::Pipe::connect(&scope.pipe).unwrap();
    let deadline = Instant::now() + server::EXCHANGE_TIMEOUT;
    server.accept(deadline).unwrap();
    client.send(&Request::Probe, deadline).unwrap();
    assert!(matches!(server.receive(deadline).unwrap(), Request::Probe));
}

#[test]
fn an_incomplete_client_cannot_hold_the_listener_forever() {
    let scope = isolated_scope();
    let server = pipe::Pipe::listen(&scope.pipe, &scope.sid).unwrap();
    let client = std::thread::spawn(move || pipe::Pipe::connect(&scope.pipe).unwrap());
    server
        .accept(Instant::now() + Duration::from_secs(1))
        .unwrap();
    let _client = client.join().unwrap();
    let started = Instant::now();
    assert!(
        server
            .receive::<Request>(started + Duration::from_millis(40))
            .is_err()
    );
    assert!(started.elapsed() < Duration::from_secs(1));
}
