use super::*;
use std::{collections::HashMap, path::PathBuf, sync::Barrier};
use windshare_activation::EntrySource;

#[derive(Clone, Default)]
struct Owner(Arc<Mutex<Owned>>);
#[derive(Default)]
struct Owned {
    selections: HashMap<IntentId, Vec<PathBuf>>,
    closing: bool,
}
impl ShareOwner for Owner {
    fn create(&self, intent: ShareIntent) -> Result<String, String> {
        let mut owned = self.0.lock().unwrap();
        assert!(!owned.closing, "admission must serialize with shutdown");
        if let Some(paths) = owned.selections.get(&intent.id) {
            if paths != &intent.roots {
                return Err("conflict".into());
            }
        } else {
            owned.selections.insert(intent.id, intent.roots);
        }
        drop(owned);
        Ok(intent.id.to_string())
    }
    fn begin_shutdown(&self) {
        self.0.lock().unwrap().closing = true;
    }
}
fn intent() -> ShareIntent {
    ShareIntent::new(
        EntrySource::Explorer,
        vec![std::env::temp_dir().join("fixture")],
    )
}
fn request(app: &Application<Owner>, intent: ShareIntent) -> Request {
    Request::Activate {
        instance: app.instance,
        activation: Activation::ShareSelection(intent),
    }
}

#[test]
fn retries_reuse_owned_selection_without_replaying_presentation() {
    let app = Application::new();
    let owner = Owner::default();
    let intent = intent();
    assert!(matches!(
        app.receive(request(&app, intent.clone())),
        Reply::Starting
    ));
    app.start(owner.clone());
    assert!(matches!(
        app.receive(request(&app, intent.clone())),
        Reply::Accepted { .. }
    ));
    assert_eq!(
        app.drain(),
        vec![Presentation::Share(intent.id.to_string())]
    );
    let generation = app.generation();
    assert!(matches!(
        app.receive(request(&app, intent.clone())),
        Reply::Accepted { .. }
    ));
    assert_eq!(app.drain(), Vec::<Presentation>::new());
    assert_eq!(app.generation(), generation);
    let mut conflicting = intent;
    conflicting.roots.push(PathBuf::from("different"));
    assert!(matches!(
        app.receive(request(&app, conflicting)),
        Reply::Rejected { .. }
    ));
    assert_eq!(app.generation(), generation);
    assert_eq!(owner.0.lock().unwrap().selections.len(), 1);
}

#[test]
fn bounded_presentation_queue_rejects_before_ownership_but_accepts_retries() {
    let app = Application::new();
    let owner = Owner::default();
    app.start(owner.clone());
    let first = intent();
    app.submit(Activation::ShareSelection(first.clone()))
        .unwrap();
    for _ in 1..PRESENTATION_CAPACITY {
        app.submit(Activation::ShareSelection(intent())).unwrap();
    }
    assert!(app.submit(Activation::ShareSelection(intent())).is_err());
    app.submit(Activation::ShareSelection(first)).unwrap();
    assert_eq!(
        owner.0.lock().unwrap().selections.len(),
        PRESENTATION_CAPACITY
    );
    assert_eq!(app.drain().len(), PRESENTATION_CAPACITY);
    app.submit(Activation::ShareSelection(intent())).unwrap();
}

#[test]
fn new_share_invalidates_pending_quit_but_committed_shutdown_seals_admission() {
    let app = Application::new();
    let owner = Owner::default();
    app.start(owner.clone());
    let generation = app.generation();
    app.submit(Activation::ShareSelection(intent())).unwrap();
    assert!(!app.commit_shutdown(generation));
    assert!(!owner.0.lock().unwrap().closing);
    assert!(app.commit_shutdown(app.generation()));
    assert!(matches!(
        app.receive(request(&app, intent())),
        Reply::Closing
    ));
    assert_eq!(owner.0.lock().unwrap().selections.len(), 1);
    assert!(matches!(
        app.receive(Request::Activate {
            instance: IntentId::new(),
            activation: Activation::ShowWindow,
        }),
        Reply::Rejected { .. }
    ));
}

#[test]
fn concurrent_shutdown_and_admission_have_one_order() {
    for _ in 0..16 {
        let app = Application::new();
        let owner = Owner::default();
        app.start(owner.clone());
        let barrier = Arc::new(Barrier::new(2));
        let closing = app.clone();
        let ready = barrier.clone();
        let thread = std::thread::spawn(move || {
            ready.wait();
            closing.commit_shutdown(0)
        });
        barrier.wait();
        let accepted = app.submit(Activation::ShareSelection(intent())).is_ok();
        let closed = thread.join().unwrap();
        assert_ne!(accepted, closed);
        assert_eq!(
            owner.0.lock().unwrap().selections.len(),
            usize::from(accepted)
        );
    }
}
