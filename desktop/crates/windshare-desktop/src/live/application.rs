use std::{
    collections::{HashSet, VecDeque},
    sync::{Arc, Mutex},
};
use windshare_activation::{
    Activation, IntentId, Phase, Reply, Request, ShareIntent, protocol::PRESENTATION_CAPACITY,
};
use windshare_host::Controller;

pub trait ShareOwner: Clone {
    fn create(&self, intent: ShareIntent) -> Result<String, String>;
    fn begin_shutdown(&self);
}
impl ShareOwner for Controller {
    fn create(&self, intent: ShareIntent) -> Result<String, String> {
        self.create(intent)
    }
    fn begin_shutdown(&self) {
        self.begin_shutdown();
    }
}

#[derive(Clone)]
pub struct Application<Owner = Controller> {
    instance: IntentId,
    state: Arc<Mutex<State<Owner>>>,
    wake: async_channel::Sender<()>,
    updates: async_channel::Receiver<()>,
}

enum Lifetime<Owner> {
    Starting,
    Running(Owner),
    Closing,
}
struct State<Owner> {
    lifetime: Lifetime<Owner>,
    pending: VecDeque<Presentation>,
    presented: HashSet<IntentId>,
    generation: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Presentation {
    ShowWindow,
    Share(String),
}

impl<Owner: ShareOwner> Application<Owner> {
    pub fn new() -> Self {
        let (wake, updates) = async_channel::bounded(1);
        Self {
            instance: IntentId::new(),
            state: Arc::new(Mutex::new(State {
                lifetime: Lifetime::Starting,
                pending: VecDeque::new(),
                presented: HashSet::new(),
                generation: 0,
            })),
            wake,
            updates,
        }
    }

    pub fn start(&self, controller: Owner) {
        let mut state = self.state.lock().expect("activation state");
        assert!(matches!(state.lifetime, Lifetime::Starting));
        state.lifetime = Lifetime::Running(controller);
        drop(state);
        tracing::info!(event = "activation.owner_running", instance = %self.instance);
    }

    pub fn receive(&self, request: Request) -> Reply {
        let mut state = self.state.lock().expect("activation state");
        match request {
            Request::Probe => Reply::Status {
                instance: self.instance,
                phase: match state.lifetime {
                    Lifetime::Starting => Phase::Starting,
                    Lifetime::Running(_) => Phase::Running,
                    Lifetime::Closing => Phase::Closing,
                },
            },
            Request::Activate {
                instance,
                activation,
            } => {
                if instance != self.instance {
                    return Reply::Rejected {
                        message: "The application session changed. Retry the share.".into(),
                    };
                }
                self.accept_locked(&mut state, activation)
            }
        }
    }

    pub fn submit(&self, activation: Activation) -> Result<(), String> {
        let mut state = self.state.lock().expect("activation state");
        match self.accept_locked(&mut state, activation) {
            Reply::Accepted { .. } => Ok(()),
            Reply::Rejected { message } => Err(message),
            Reply::Starting => Err("WindShare is starting".into()),
            _ => Err("WindShare is closing".into()),
        }
    }

    fn accept_locked(&self, state: &mut State<Owner>, activation: Activation) -> Reply {
        let controller = match &state.lifetime {
            Lifetime::Starting => return Reply::Starting,
            Lifetime::Closing => return Reply::Closing,
            Lifetime::Running(controller) => controller,
        };
        let presentation = match &activation {
            Activation::ShowWindow => Presentation::ShowWindow,
            Activation::ShareSelection(intent) => Presentation::Share(intent.id.to_string()),
        };
        let duplicate = matches!(&activation, Activation::ShareSelection(intent) if state.presented.contains(&intent.id));
        let already_queued = duplicate || state.pending.contains(&presentation);
        if !already_queued && state.pending.len() >= PRESENTATION_CAPACITY {
            return Reply::Rejected {
                message: "WindShare is handling too many requests. Try again shortly.".into(),
            };
        }
        if let Activation::ShareSelection(intent) = activation {
            let id = intent.id;
            tracing::info!(event = "activation.accepting", instance = %self.instance,
                intent_id = %id, source = ?intent.source, count = intent.roots.len(), duplicate);
            // The owner verifies the selection even for a retry, so reusing an ID
            // cannot conceal different content. Only new ownership invalidates UI callbacks.
            if let Err(message) = controller.create(intent) {
                return Reply::Rejected { message };
            }
            if state.presented.insert(id) {
                state.generation += 1;
            }
        }
        if !already_queued {
            state.pending.push_back(presentation);
            let _ = self.wake.try_send(());
        }
        Reply::Accepted {
            instance: self.instance,
        }
    }

    pub fn drain(&self) -> Vec<Presentation> {
        self.state
            .lock()
            .expect("activation state")
            .pending
            .drain(..)
            .collect()
    }

    pub async fn changed(&self) {
        let _ = self.updates.recv().await;
    }

    pub fn generation(&self) -> u64 {
        self.state.lock().expect("activation state").generation
    }

    pub fn commit_shutdown(&self, generation: u64) -> bool {
        let mut state = self.state.lock().expect("activation state");
        if state.generation != generation {
            return false;
        }
        self.close_locked(&mut state);
        drop(state);
        true
    }

    pub fn begin_shutdown(&self) {
        self.close_locked(&mut self.state.lock().expect("activation state"));
    }

    fn close_locked(&self, state: &mut State<Owner>) {
        if let Lifetime::Running(controller) = &state.lifetime {
            controller.begin_shutdown();
        }
        state.lifetime = Lifetime::Closing;
        tracing::info!(event = "activation.owner_closing", instance = %self.instance);
    }
}

#[cfg(test)]
#[path = "application_tests.rs"]
mod tests;
