extern crate std;

use core::{num::NonZeroU64, time::Duration};
use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::*;
use crate::{
    challenge::ChallengeSeed,
    descriptor_upload::DescriptorUpload,
    handshake::{Admission, AdmissionRequest, CloseReason, ControlHandshake, Outcome, Step},
    registration::RegisterInit,
    stop::StopInit,
    test_vectors::{Decimal, bytes, challenge_seed, relay_identity, session_case, unix_seconds},
};

const VECTOR_REGISTRATION_MILLIS: u64 = 5_000;
// WS2R carries the instance right after the eight-byte header and share ID.
const INIT_INSTANCE_OFFSET: usize = 8 + 12;

#[derive(Default, Debug)]
struct Recorder(Vec<Observation>);

impl Observer for Recorder {
    fn observe(&mut self, observation: Observation) {
        self.0.push(observation);
    }
}

fn connection(id: u64, generation: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::new(generation).unwrap(),
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Vector {
    relay_identity_b64: String,
    challenge_id_b64: String,
    challenge_nonce_b64: String,
    expires_at: Decimal,
    register_init_b64: String,
    register_proof_b64: String,
    descriptor_upload_b64: String,
    registered_b64: String,
    resume_challenge_id_b64: String,
    resume_challenge_nonce_b64: String,
    resume_expires_at: Decimal,
    resume_init_b64: String,
    resume_credential_b64: String,
    resume_proof_b64: String,
    stop_init_b64: String,
}

struct Fixture {
    vector: Vector,
    upload: Vec<u8>,
    /// When a fresh handshake must start for the shared challenge expiry.
    at: UnixMillis,
    /// When a resume handshake must start for its shared challenge expiry.
    resume_at: UnixMillis,
}

/// When a registration handshake must start for its deadline to round up to
/// the shared challenge expiry.
fn registration_start(expires_at: &str) -> UnixMillis {
    UnixMillis(unix_seconds(expires_at).start().0 - VECTOR_REGISTRATION_MILLIS - 1)
}

impl Fixture {
    fn new() -> Self {
        let vector: Vector = session_case("fresh-relay-registration-proof");
        let at = registration_start(&vector.expires_at);
        let resume_at = registration_start(&vector.resume_expires_at);
        let upload = bytes(&vector.descriptor_upload_b64);
        Self {
            vector,
            upload,
            at,
            resume_at,
        }
    }

    fn relay(&self) -> RelayIdentity {
        relay_identity(&self.vector.relay_identity_b64)
    }

    fn init(&self) -> RegisterInit {
        RegisterInit::decode(&bytes(&self.vector.register_init_b64)).unwrap()
    }

    fn seed(&self) -> ChallengeSeed {
        challenge_seed(
            &self.vector.challenge_id_b64,
            &self.vector.challenge_nonce_b64,
        )
    }

    fn share(&self) -> ShareId {
        self.init().registration().route().share_id()
    }

    fn engine(&self) -> ShareLifecycle<Recorder> {
        let (engine, action) = self
            .restore(None, Incarnation::new([1; 16]).unwrap())
            .unwrap();
        assert_eq!(action, Action::None);
        engine
    }

    /// Restores after a process restart, when no connection survives.
    fn restore(
        &self,
        stored: Option<Record>,
        incarnation: Incarnation,
    ) -> Result<(ShareLifecycle<Recorder>, Action<'static>), RestoreError> {
        self.restore_at(stored, &[], incarnation, self.at)
    }

    /// Restores through persisted bytes, as a host reads its storage.
    fn restore_at(
        &self,
        stored: Option<Record>,
        surviving: &[ConnectionRef],
        incarnation: Incarnation,
        at: UnixMillis,
    ) -> Result<(ShareLifecycle<Recorder>, Action<'static>), RestoreError> {
        let stored = stored.map(|record| Record::decode(record.encode().as_bytes()).unwrap());
        ShareLifecycle::restore(
            self.relay(),
            self.share(),
            stored,
            surviving.iter().copied(),
            incarnation,
            Recorder::default(),
            at,
        )
    }

    fn handshake(&self, connection: ConnectionRef, at: UnixMillis) -> ControlHandshake {
        let mut handshake = ControlHandshake::open(self.relay(), connection, at);
        assert_eq!(
            handshake.receive(&bytes(&self.vector.register_init_b64), at),
            Step::RequestAdmission(AdmissionRequest::Registration(self.init()))
        );
        handshake
    }

    fn reserve(
        &self,
        engine: &mut ShareLifecycle<Recorder>,
        connection: ConnectionRef,
        at: UnixMillis,
    ) -> Reservation {
        let handshake = self.handshake(connection, at);
        let effects = engine.handle(
            Event::Reserve {
                connection,
                init: self.init(),
                deadline: handshake.deadline().unwrap(),
            },
            at,
        );
        assert_eq!(effects.expiration, None);
        match effects.action {
            Action::Reserved(reservation) => reservation,
            action => panic!("expected reservation: {action:?}"),
        }
    }

    fn admit(&self, reservation: Reservation) -> ControlHandshake {
        let mut handshake = self.handshake(reservation.id().connection(), self.at);
        assert!(matches!(
            handshake.admit(Admission::Fresh(reservation), self.seed(), self.at),
            Step::SendChallenge(_)
        ));
        handshake
    }

    fn authenticate(&self, reservation: Reservation) -> Publication<'_> {
        let mut handshake = self.admit(reservation);
        assert_eq!(
            handshake.receive(&bytes(&self.vector.register_proof_b64), self.at),
            Step::AwaitFrame
        );
        match handshake.receive(&self.upload, self.at) {
            Step::Complete(Outcome::Publish(publication)) => publication,
            step => panic!("expected authenticated upload: {step:?}"),
        }
    }

    fn publish<'a>(&'a self, engine: &mut ShareLifecycle<Recorder>) -> PublishRequest<'a> {
        let reservation = self.reserve(engine, connection(1, 1), self.at);
        let publication = self.authenticate(reservation);
        let effects = engine.handle(Event::Publish(publication), self.at);
        assert_eq!(effects.expiration, None);
        match effects.action {
            Action::Publish(request) => request,
            action => panic!("expected host transaction: {action:?}"),
        }
    }

    /// Publishes the shared registration, owned by `connection(1, 1)`.
    fn register(&self, engine: &mut ShareLifecycle<Recorder>) -> PublishedRegistration {
        let request = self.publish(engine);
        let finished = Event::PublicationFinished {
            id: request.id,
            result: PublicationResult::Committed,
        };
        match engine.handle(finished, self.at).action {
            Action::Registered(published) => published,
            action => panic!("publication must register: {action:?}"),
        }
    }

    fn join(&self, engine: &mut ShareLifecycle<Recorder>, at: UnixMillis) -> Effects<'static> {
        engine.handle(Event::Join(self.share()), at)
    }

    fn reserve_event(&self, at: UnixMillis) -> Event<'static> {
        Event::Reserve {
            connection: connection(1, 1),
            init: self.init(),
            deadline: at.saturating_add(Duration::from_millis(VECTOR_REGISTRATION_MILLIS)),
        }
    }

    fn resume_init(&self) -> RegisterInit {
        RegisterInit::decode(&bytes(&self.vector.resume_init_b64)).unwrap()
    }

    /// The same resume, naming another instance of the share.
    fn foreign_resume_init(&self) -> RegisterInit {
        let mut init = bytes(&self.vector.resume_init_b64);
        init[INIT_INSTANCE_OFFSET] ^= 1;
        RegisterInit::decode(&init).unwrap()
    }

    /// Requests resume admission with the deadline of a handshake opened at
    /// `resume_at`, the only start its shared challenge allows.
    fn resume_event(&self, connection: ConnectionRef) -> Event<'static> {
        Event::BeginResume {
            connection,
            init: self.resume_init(),
            deadline: self
                .resume_at
                .saturating_add(Duration::from_millis(VECTOR_REGISTRATION_MILLIS)),
        }
    }

    fn begin_resume(
        &self,
        engine: &mut ShareLifecycle<Recorder>,
        connection: ConnectionRef,
    ) -> ResumeAttempt {
        match engine
            .handle(self.resume_event(connection), self.resume_at)
            .action
        {
            Action::ResumeAdmitted(attempt) => attempt,
            action => panic!("expected resume admission: {action:?}"),
        }
    }

    /// Runs the shared resume handshake on the attempt's connection.
    fn authenticate_resume(&self, attempt: ResumeAttempt) -> Resumption {
        let at = self.resume_at;
        let mut handshake = ControlHandshake::open(self.relay(), attempt.id().connection(), at);
        assert_eq!(
            handshake.receive(&bytes(&self.vector.resume_init_b64), at),
            Step::AwaitFrame
        );
        assert_eq!(
            handshake.receive(&bytes(&self.vector.resume_credential_b64), at),
            Step::RequestAdmission(AdmissionRequest::Registration(self.resume_init()))
        );
        let seed = challenge_seed(
            &self.vector.resume_challenge_id_b64,
            &self.vector.resume_challenge_nonce_b64,
        );
        assert!(matches!(
            handshake.admit(Admission::Resume(attempt), seed, at),
            Step::SendChallenge(_)
        ));
        match handshake.receive(&bytes(&self.vector.resume_proof_b64), at) {
            Step::Complete(Outcome::Resume(resumption)) => resumption,
            step => panic!("expected authenticated resume: {step:?}"),
        }
    }

    fn resume(
        &self,
        engine: &mut ShareLifecycle<Recorder>,
        connection: ConnectionRef,
    ) -> Effects<'static> {
        let resumption = self.authenticate_resume(self.begin_resume(engine, connection));
        engine.handle(Event::Resume(resumption), self.resume_at)
    }

    /// The STOP record the shared stop vector would store.
    fn stopped(&self) -> Record {
        let stop = StopInit::decode(&bytes(&self.vector.stop_init_b64)).unwrap();
        Record::Stopped(StoppedShare {
            relay: stop.relay(),
            route: stop.route(),
            share_instance: stop.share_instance(),
            stop_id: stop.stop_id(),
        })
    }
}

#[test]
fn publication_requires_host_commit_and_preparation_before_join_or_registered() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::Refused(Refusal::NotFound)
    );
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let id = reservation.id();
    let deadline = reservation.deadline();
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::Refused(Refusal::Starting)
    );
    let publication = fixture.authenticate(reservation);
    let Action::Publish(request) = engine
        .handle(Event::Publish(publication), fixture.at)
        .action
    else {
        panic!("authenticated upload must request a host transaction");
    };
    assert_eq!(request.id, id);
    assert_eq!(request.init, fixture.init());
    assert_eq!(request.deadline, deadline);
    assert_eq!(
        request.descriptor,
        DescriptorUpload::decode(&fixture.upload).unwrap().object()
    );
    assert_eq!(engine.phase(), Phase::Publishing);
    assert_eq!(engine.deadline(), Some(deadline));
    assert_eq!(
        fixture.join(&mut engine, fixture.at).action,
        Action::Refused(Refusal::Starting)
    );

    // The fake host retains both resources before returning committed evidence.
    let stored_descriptor = request.descriptor.to_vec();
    let prepared_connection = request.id.connection();
    let Action::Registered(published) = engine
        .handle(
            Event::PublicationFinished {
                id,
                result: PublicationResult::Committed,
            },
            UnixMillis(deadline.0 - 1),
        )
        .action
    else {
        panic!("confirmed publication must acknowledge the sender");
    };
    assert_eq!(
        published.acknowledgement().encode().as_slice(),
        bytes(&fixture.vector.registered_b64)
    );
    assert_eq!(published.id.connection(), prepared_connection);
    assert_eq!(
        published.route.registration().descriptor_digest(),
        crate::identifiers::DescriptorDigest::of(&stored_descriptor)
    );
    assert_eq!(
        fixture.join(&mut engine, deadline).action,
        Action::JoinReady(published)
    );
    assert_eq!(engine.phase(), Phase::Published);
    assert_eq!(engine.deadline(), None);
    assert_eq!(engine.handle(Event::Tick, deadline).expiration, None);
}

#[test]
fn competing_registration_never_changes_the_incumbent() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let deadline = reservation.deadline();
    let other = Event::Reserve {
        connection: connection(2, 1),
        init: fixture.init(),
        deadline,
    };
    assert_eq!(
        engine.handle(other, fixture.at).action,
        Action::Refused(Refusal::Occupied)
    );
    assert_eq!(engine.deadline(), Some(deadline));
    let request = engine.handle(
        Event::Publish(fixture.authenticate(reservation)),
        fixture.at,
    );
    let Action::Publish(request) = request.action else {
        panic!("incumbent remains admitted");
    };
    assert_eq!(
        engine
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::Occupied)
    );
    assert!(matches!(
        engine
            .handle(
                Event::PublicationFinished {
                    id: request.id,
                    result: PublicationResult::Committed
                },
                fixture.at
            )
            .action,
        Action::Registered(_)
    ));
    assert_eq!(
        engine
            .handle(fixture.reserve_event(fixture.at), fixture.at)
            .action,
        Action::Refused(Refusal::Occupied)
    );
}

#[test]
fn failed_handshake_releases_only_its_attempt() {
    let fixture = Fixture::new();
    for fail_descriptor in [false, true] {
        let mut engine = fixture.engine();
        let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
        let id = reservation.id();
        let mut handshake = fixture.admit(reservation);
        let mut proof = bytes(&fixture.vector.register_proof_b64);
        if fail_descriptor {
            assert_eq!(handshake.receive(&proof, fixture.at), Step::AwaitFrame);
            let mut upload = fixture.upload.clone();
            *upload.last_mut().unwrap() ^= 1;
            assert_eq!(
                handshake.receive(&upload, fixture.at),
                Step::Close(CloseReason::DescriptorMismatch)
            );
        } else {
            *proof.last_mut().unwrap() ^= 1;
            assert!(matches!(
                handshake.receive(&proof, fixture.at),
                Step::Close(CloseReason::Unauthenticated(_))
            ));
        }
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id,
                        reason: Cancellation::HandshakeFailed
                    },
                    fixture.at
                )
                .action,
            Action::Released(id)
        );
        let replacement = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
        assert_ne!(id, replacement.id());
        assert_eq!(
            engine
                .handle(
                    Event::Cancel {
                        id,
                        reason: Cancellation::HandshakeFailed
                    },
                    fixture.at
                )
                .action,
            Action::Refused(Refusal::StaleOperation)
        );
        assert_eq!(engine.phase(), Phase::Reserved);
    }
}

#[test]
fn expired_reservation_is_released_at_the_exact_boundary_on_any_event() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let id = reservation.id();
    let deadline = reservation.deadline();
    assert_eq!(
        engine
            .handle(Event::Tick, UnixMillis(deadline.0 - 1))
            .expiration,
        None
    );
    let effects = engine.handle(fixture.reserve_event(deadline), deadline);
    assert_eq!(effects.expiration, Some(Expiration::Released(id)));
    let Action::Reserved(replacement) = effects.action else {
        panic!("expired capacity must be reusable");
    };
    assert_ne!(replacement.id(), id);
    assert!(replacement.deadline() > deadline);
    assert_eq!(engine.handle(Event::Tick, deadline).expiration, None);
    assert_eq!(
        engine
            .handle(Event::Publish(fixture.authenticate(reservation)), deadline)
            .action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(engine.deadline(), Some(replacement.deadline()));
}

#[test]
fn delayed_authenticated_upload_cannot_use_a_new_attempt_on_the_same_connection() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let old = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let id = old.id();
    let upload = fixture.authenticate(old);
    assert_eq!(
        engine
            .handle(
                Event::Cancel {
                    id,
                    reason: Cancellation::Disconnected
                },
                fixture.at
            )
            .action,
        Action::Released(id)
    );
    let replacement = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    assert_eq!(
        engine.handle(Event::Publish(upload), fixture.at).action,
        Action::Refused(Refusal::StaleOperation)
    );
    assert_eq!(engine.phase(), Phase::Reserved);
    assert!(matches!(
        engine
            .handle(
                Event::Publish(fixture.authenticate(replacement)),
                fixture.at
            )
            .action,
        Action::Publish(_)
    ));
}

#[test]
fn structured_observations_correlate_expiry_and_the_replacement() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let reservation = fixture.reserve(&mut engine, connection(1, 1), fixture.at);
    let id = reservation.id();
    let deadline = reservation.deadline();
    let effects = engine.handle(fixture.reserve_event(deadline), deadline);
    let Action::Reserved(replacement) = effects.action else {
        panic!("replacement");
    };
    let observations = &engine.observer.0;
    assert_eq!(observations.len(), 4);
    let restored = observations[0];
    assert_eq!(restored.event, ObservedEvent::Restore);
    assert_eq!(
        (restored.before, restored.after),
        (Phase::Vacant, Phase::Vacant)
    );
    assert_eq!((restored.operation, restored.connection), (None, None));
    assert_eq!(observations[1].operation, Some(id));
    assert_eq!(observations[1].before, Phase::Vacant);
    assert_eq!(observations[1].after, Phase::Reserved);
    assert_eq!(observations[2].event, ObservedEvent::Deadline);
    assert_eq!(observations[2].operation, Some(id));
    assert_eq!(observations[2].decision, Decision::Released);
    assert_eq!(observations[3].operation, Some(replacement.id()));
    for observation in observations {
        assert_eq!(observation.relay, fixture.relay());
        assert_eq!(observation.share, fixture.share());
        assert_eq!(observation.incarnation, id.incarnation());
    }
    for observation in &observations[1..] {
        assert_eq!(observation.connection, Some(connection(1, 1)));
    }
}

#[path = "publication_tests.rs"]
mod publication;

#[path = "admission_tests.rs"]
mod admission;

#[path = "restoration_tests.rs"]
mod restoration;

#[path = "ownership_tests.rs"]
mod ownership;

#[path = "stopping_tests.rs"]
mod stopping;
