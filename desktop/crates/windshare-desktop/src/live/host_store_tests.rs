use super::*;

fn snapshot() -> Arc<Model> {
    Arc::new(Model::default())
}

#[test]
fn non_model_notification_emits_state_changed_without_replaying_prior_edge() {
    let initial = snapshot();
    let updated = snapshot();
    let mut cursor = ModelCursor::new(&initial);

    assert!(matches!(cursor.observe(&initial), HostChange::StateChanged));

    match cursor.observe(&updated) {
        HostChange::ModelChanged { previous, current } => {
            assert!(Arc::ptr_eq(&previous, &initial));
            assert!(Arc::ptr_eq(&current, &updated));
        }
        HostChange::StateChanged => panic!("expected model transition"),
    }

    // Re-notifying with the same snapshot (as HostStore::set_exiting does) must
    // report a non-model state change instead of replaying (initial, updated).
    assert!(matches!(cursor.observe(&updated), HostChange::StateChanged));

    let next = snapshot();
    match cursor.observe(&next) {
        HostChange::ModelChanged { previous, current } => {
            assert!(Arc::ptr_eq(&previous, &updated));
            assert!(Arc::ptr_eq(&current, &next));
        }
        HostChange::StateChanged => panic!("expected subsequent model transition"),
    }
}

#[test]
fn independent_cursors_isolate_late_subscribers_and_coalesced_updates() {
    let m0 = snapshot();
    let m1 = snapshot();
    let m2 = snapshot();

    let mut early = ModelCursor::new(&m0);
    // A late subscriber created at m1 must not observe the earlier m0 -> m1 edge.
    let mut late = ModelCursor::new(&m1);
    assert!(matches!(late.observe(&m1), HostChange::StateChanged));

    // When m1 is coalesced before delivery, the early subscriber still observes
    // its true m0 -> m2 transition while the late subscriber observes m1 -> m2.
    match early.observe(&m2) {
        HostChange::ModelChanged { previous, current } => {
            assert!(Arc::ptr_eq(&previous, &m0));
            assert!(Arc::ptr_eq(&current, &m2));
        }
        HostChange::StateChanged => panic!("expected coalesced transition for early cursor"),
    }
    match late.observe(&m2) {
        HostChange::ModelChanged { previous, current } => {
            assert!(Arc::ptr_eq(&previous, &m1));
            assert!(Arc::ptr_eq(&current, &m2));
        }
        HostChange::StateChanged => panic!("expected transition for late cursor"),
    }
}
