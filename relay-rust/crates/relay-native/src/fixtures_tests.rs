use crate::{config::limits, environment::Trace};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::num::NonZeroU64;
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, Incarnation},
    registration::RegisterInit,
    share::{
        Identity, Input, Output, RelayStep, Responsibility, Restore, ShareRelay, Storage, Work,
        WorkResult,
    },
};

pub struct Fixture(pub serde_json::Value);

impl Fixture {
    pub fn new() -> Self {
        let vectors: serde_json::Value =
            serde_json::from_str(include_str!("../../../../core/testvectors/v2-session.json"))
                .unwrap();
        Self(
            vectors["cases"]
                .as_array()
                .unwrap()
                .iter()
                .find(|case| case["name"] == "relay-capacity-registration")
                .unwrap()
                .clone(),
        )
    }
    pub fn bytes(&self, key: &str) -> Vec<u8> {
        STANDARD.decode(self.0[key].as_str().unwrap()).unwrap()
    }
    pub fn now(&self, stop: bool) -> UnixMillis {
        let field = if stop { "stopExpiresAt" } else { "expiresAt" };
        let budget = if stop { 10_000 } else { 5_000 };
        UnixMillis(self.0[field].as_str().unwrap().parse::<u64>().unwrap() * 1000 - budget - 1)
    }
    pub fn seed(&self, stop: bool) -> ChallengeSeed {
        let fields = if stop {
            ["stopChallengeIdB64", "stopChallengeNonceB64"]
        } else {
            ["challengeIdB64", "challengeNonceB64"]
        };
        ChallengeSeed::new(
            self.bytes(fields[0]).try_into().unwrap(),
            self.bytes(fields[1]).try_into().unwrap(),
        )
        .unwrap()
    }
    pub fn identity(&self) -> Identity {
        let init = RegisterInit::decode(&self.bytes("registerInitB64")).unwrap();
        Identity {
            relay: RelayIdentity::new(self.bytes("relayIdentityB64").try_into().unwrap()).unwrap(),
            share: init.registration().route().share_id(),
            incarnation: Incarnation::new([9; 16]).unwrap(),
        }
    }
    pub fn works(&self) -> Vec<Storage> {
        let (mut relay, _) = ShareRelay::restore(
            self.identity(),
            limits(),
            &Restore::restarted(None, Responsibility::Released { previous: None }),
            Trace,
            self.now(true),
        )
        .unwrap();
        self.begin(&mut relay, true);
        self.begin(&mut relay, false);
        let _ = relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &self.bytes("registerProofB64"),
            },
            self.now(false),
        );
        let publication = work(relay.handle(
            Input::Control {
                connection: connection(1),
                frame: &self.bytes("descriptorUploadB64"),
            },
            self.now(false),
        ));
        let _ = relay.handle(
            Input::Completed {
                id: publication.id,
                result: WorkResult::Complete,
            },
            self.now(false),
        );
        let stopping = work(relay.handle(
            Input::Control {
                connection: connection(2),
                frame: &self.bytes("stopProofB64"),
            },
            self.now(false),
        ));
        let stopped = work(relay.handle(
            Input::Completed {
                id: stopping.id,
                result: WorkResult::Complete,
            },
            self.now(false),
        ));
        vec![publication.operation, stopping.operation, stopped.operation]
    }
    pub fn begin(&self, relay: &mut ShareRelay<Trace>, stop: bool) {
        let connection = connection(if stop { 2 } else { 1 });
        let now = self.now(stop);
        let _ = relay.handle(Input::Connected(connection), now);
        let init = self.bytes(if stop {
            "stopInitB64"
        } else {
            "registerInitB64"
        });
        let input = if stop {
            Input::Control {
                connection,
                frame: &init,
            }
        } else {
            Input::Register(
                windshare_relay_core::access::RegistrationRequest::from_frame(connection, &init)
                    .unwrap()
                    .resolve(
                        windshare_relay_core::access::RegistrationDecision::Granted(
                            windshare_relay_core::access::Attribution::Anonymous,
                        ),
                        now,
                    ),
            )
        };
        let outcome = relay.handle(input, now);
        let request = outcome
            .outputs
            .into_iter()
            .find_map(|output| match output {
                Output::ChallengeNeeded(request) => Some(request),
                _ => None,
            })
            .unwrap();
        let _ = relay.handle(
            Input::Challenge {
                request,
                seed: self.seed(stop),
            },
            now,
        );
    }
}

fn work(step: RelayStep) -> Work {
    assert!(step.refusal.is_none(), "{:?}", step.refusal);
    step.outputs
        .into_iter()
        .find_map(|output| match output {
            Output::Work(work) => Some(work),
            _ => None,
        })
        .unwrap()
}

pub fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::MIN,
    }
}

pub fn socket_admission() -> std::sync::Arc<crate::connection::Admission> {
    crate::connection::Capacity::new(
        crate::connection::ConnectionLimits::default(),
        crate::config::CONNECTION_ATTEMPTS,
        windshare_relay_core::clock::UnixMillis(0),
    )
    .unwrap()
    .accept(
        "127.0.0.1".parse().unwrap(),
        connection(1),
        windshare_relay_core::clock::UnixMillis(0),
        crate::connection::AdmissionPriority::Authenticated,
    )
    .unwrap()
}

mod trace_capture {
    use std::{
        cell::RefCell,
        future::Future,
        io,
        sync::{
            Arc, Mutex, Once,
            atomic::{AtomicBool, Ordering},
        },
    };
    use tracing::{
        Event, Level, Metadata, Subscriber, level_filters::LevelFilter, span, subscriber::Interest,
    };

    #[derive(Clone, Default)]
    struct LogBuffer(Arc<Mutex<Vec<u8>>>);

    impl io::Write for LogBuffer {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    thread_local! {
        static CAPTURE_BUFFER: RefCell<Option<LogBuffer>> = const { RefCell::new(None) };
    }

    enum OptionalWriter {
        Active(LogBuffer),
        Sink,
    }

    impl io::Write for OptionalWriter {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            match self {
                Self::Active(buffer) => buffer.write(buf),
                Self::Sink => Ok(buf.len()),
            }
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    struct ThreadLocalWriter;

    impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for ThreadLocalWriter {
        type Writer = OptionalWriter;

        fn make_writer(&'a self) -> Self::Writer {
            CAPTURE_BUFFER.with(|slot| {
                slot.borrow()
                    .as_ref()
                    .cloned()
                    .map_or(OptionalWriter::Sink, OptionalWriter::Active)
            })
        }
    }

    // `tracing_core` optimizes single-dispatcher callsite registration by querying
    // the calling thread's default subscriber (`Rebuilder::JustOne`). Under
    // parallel `cargo test`, a thread-local `with_default` lets sibling threads
    // without a subscriber register a new callsite as `Interest::never()` and drop
    // events on the capturing thread. A single global subscriber that routes by
    // thread-local capture buffer avoids both cross-thread callsite poisoning and
    // cross-test log pollution.
    struct CaptureSubscriber<S> {
        ready: Arc<AtomicBool>,
        inner: S,
    }

    impl<S: Subscriber> Subscriber for CaptureSubscriber<S> {
        fn register_callsite(&self, metadata: &'static Metadata<'static>) -> Interest {
            let _ = self.inner.register_callsite(metadata);
            if *metadata.level() <= Level::INFO {
                Interest::sometimes()
            } else {
                Interest::never()
            }
        }

        fn max_level_hint(&self) -> Option<LevelFilter> {
            Some(if self.ready.load(Ordering::Acquire) {
                LevelFilter::INFO
            } else {
                LevelFilter::OFF
            })
        }

        fn enabled(&self, metadata: &Metadata<'_>) -> bool {
            *metadata.level() <= Level::INFO
                && CAPTURE_BUFFER.with(|slot| slot.borrow().is_some())
                && self.inner.enabled(metadata)
        }

        fn event_enabled(&self, event: &Event<'_>) -> bool {
            CAPTURE_BUFFER.with(|slot| slot.borrow().is_some()) && self.inner.event_enabled(event)
        }

        fn new_span(&self, span: &span::Attributes<'_>) -> span::Id {
            self.inner.new_span(span)
        }

        fn record(&self, span: &span::Id, values: &span::Record<'_>) {
            self.inner.record(span, values);
        }

        fn record_follows_from(&self, span: &span::Id, follows: &span::Id) {
            self.inner.record_follows_from(span, follows);
        }

        fn event(&self, event: &Event<'_>) {
            self.inner.event(event);
        }

        fn enter(&self, span: &span::Id) {
            self.inner.enter(span);
        }

        fn exit(&self, span: &span::Id) {
            self.inner.exit(span);
        }

        fn clone_span(&self, id: &span::Id) -> span::Id {
            self.inner.clone_span(id)
        }

        fn try_close(&self, id: span::Id) -> bool {
            self.inner.try_close(id)
        }
    }

    fn install() {
        static INIT: Once = Once::new();
        INIT.call_once(|| {
            // Keep the global max level `OFF` until `set_global_default` completes
            // so sibling test threads cannot register callsites against
            // `NoSubscriber` in the gap between `Dispatch::new` and publication.
            let ready = Arc::new(AtomicBool::new(false));
            let inner = tracing_subscriber::fmt()
                .json()
                .with_writer(ThreadLocalWriter)
                .with_max_level(Level::INFO)
                .finish();
            let dispatch = tracing::Dispatch::new(CaptureSubscriber {
                ready: Arc::clone(&ready),
                inner,
            });
            tracing::dispatcher::set_global_default(dispatch)
                .expect("test tracing capture subscriber must be the sole global default");
            ready.store(true, Ordering::SeqCst);
            tracing::callsite::rebuild_interest_cache();
        });
    }

    struct CaptureGuard(Option<LogBuffer>);

    impl CaptureGuard {
        fn enter(buffer: LogBuffer) -> Self {
            Self(CAPTURE_BUFFER.with(|slot| slot.replace(Some(buffer))))
        }
    }

    impl Drop for CaptureGuard {
        fn drop(&mut self) {
            let _ = CAPTURE_BUFFER.try_with(|slot| slot.replace(self.0.take()));
        }
    }

    fn parse_buffer(buffer: &LogBuffer) -> Vec<serde_json::Value> {
        let bytes = buffer.0.lock().unwrap().clone();
        String::from_utf8(bytes)
            .unwrap()
            .lines()
            .map(|line| serde_json::from_str::<serde_json::Value>(line).unwrap())
            .collect()
    }

    pub fn capture_events(exercise: impl FnOnce()) -> Vec<serde_json::Value> {
        install();
        let buffer = LogBuffer::default();
        let guard = CaptureGuard::enter(buffer.clone());
        exercise();
        drop(guard);
        parse_buffer(&buffer)
    }

    pub async fn capture_events_async<F: Future<Output = ()>>(
        exercise: F,
    ) -> Vec<serde_json::Value> {
        install();
        let buffer = LogBuffer::default();
        let guard = CaptureGuard::enter(buffer.clone());
        exercise.await;
        drop(guard);
        parse_buffer(&buffer)
    }
}

pub use trace_capture::{capture_events, capture_events_async};
