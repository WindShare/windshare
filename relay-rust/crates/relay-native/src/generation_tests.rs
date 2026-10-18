use super::*;
use crate::test_support::Fixture;
use tokio::sync::oneshot;

#[tokio::test]
async fn panic_keeps_descendants_fenced_and_release_evidence_is_identity_scoped() {
    let fixture = Fixture::new();
    let identity = fixture.identity();
    let directory = tempfile::tempdir().unwrap();
    let store = Database::open(&directory.path().join("authority.sqlite"), identity.relay).unwrap();
    let generation = Generation::new(identity, store);
    let (release, pending) = oneshot::channel::<()>();
    let child = tokio::spawn(generation.track(async {
        let _ = pending.await;
    }));
    let (events, inbox) = mpsc::channel::<Event>(1);
    let actor: JoinHandle<Result<()>> = tokio::spawn(generation.track(async move {
        let _inbox = inbox;
        panic!("injected actor panic");
    }));
    generation.supervise(actor);
    events.closed().await;
    assert!(generation.recovery().is_err());
    release.send(()).unwrap();
    child.await.unwrap();
    generation.wait_released().await;
    let replacement = Identity {
        incarnation: crate::environment::incarnation().unwrap(),
        ..identity
    };
    assert!(
        generation
            .recovery()
            .unwrap()
            .restore(replacement, None)
            .is_ok()
    );
    assert!(
        generation
            .recovery()
            .unwrap()
            .restore(identity, None)
            .is_err()
    );
    let unrelated = Identity {
        share: windshare_relay_core::identifiers::ShareId::new([3; 12]).unwrap(),
        ..replacement
    };
    assert!(
        generation
            .recovery()
            .unwrap()
            .restore(unrelated, None)
            .is_err()
    );
}
