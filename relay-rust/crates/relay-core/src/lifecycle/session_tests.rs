use super::*;
use crate::{
    WIRE_VERSION,
    identifiers::RelaySessionId,
    join::JoinRequest,
    sessions::{
        self, Action as SessionAction, Direction, EndReason, Event as SessionEvent, ForwardPermit,
        ForwardRefusal, OpaqueHeader, Phase as SessionPhase, RetirementReason, Session,
        SessionAdmitted,
    },
    wire::Writer,
};

#[derive(Default)]
struct SessionTrace(Vec<sessions::Observation>);

impl sessions::Observer for SessionTrace {
    fn observe(&mut self, observation: crate::observation::Observation) {
        let crate::observation::Observation::Session(observation) = observation else {
            panic!("unexpected domain observation");
        };
        self.0.push(observation);
    }
}

fn request(fixture: &Fixture, receiver: ConnectionRef, now: UnixMillis) -> JoinRequest {
    let mut handshake = ControlHandshake::open(fixture.relay(), receiver, now);
    match handshake.receive(&bytes(&fixture.vector.join_b64), now) {
        Step::Complete(Outcome::Join(request)) => request,
        step => panic!("expected a connection-bound JOIN: {step:?}"),
    }
}

fn wire_id(value: u64) -> RelaySessionId {
    RelaySessionId::new(value.to_be_bytes()).unwrap()
}

fn join(
    fixture: &Fixture,
    engine: &mut ShareLifecycle<Recorder>,
    receiver: ConnectionRef,
    session: u64,
    at: UnixMillis,
) -> Session {
    let input = Event::Join {
        request: request(fixture, receiver, at),
        session: wire_id(session),
    };
    let effects = engine.handle(input, at);
    assert_eq!(effects.revocation, None);
    let Action::Joined(joined) = effects.action else {
        panic!("expected joined session: {:?}", effects.action);
    };
    assert_eq!(joined.registration.owner, joined.session.key().owner());
    joined.session
}

fn frame(session: &Session, source: ConnectionRef) -> SessionEvent {
    let mut writer = Writer::<21>::with_header(*b"WS2O", 0);
    writer
        .put(session.key().wire_id().as_bytes())
        .put(&1_u32.to_be_bytes())
        .put(&[0xff]);
    SessionEvent::Frame {
        source,
        header: OpaqueHeader::decode(&writer.into_frame()).unwrap(),
    }
}

fn admitted(session: &Session, sender: ConnectionRef) -> SessionEvent {
    let mut writer = Writer::<16>::with_header(*b"WS2M", 0);
    writer.put(session.key().wire_id().as_bytes());
    SessionEvent::Admit {
        sender,
        frame: SessionAdmitted::decode(&writer.into_frame()).unwrap(),
    }
}

fn forward(
    engine: &ShareLifecycle<Recorder>,
    session: &mut Session,
    source: ConnectionRef,
    at: UnixMillis,
) -> ForwardPermit {
    let event = frame(session, source);
    let effects = session.handle(engine, event, at, &mut ());
    assert_eq!(effects.retirement, None);
    let SessionAction::Forward(permit) = effects.action else {
        panic!("expected forwarding: {effects:?}");
    };
    permit
}

fn activate(engine: &ShareLifecycle<Recorder>, session: &mut Session, at: UnixMillis) {
    forward(engine, session, session.key().receiver(), at);
    let event = admitted(session, session.key().sender());
    assert_eq!(
        session.handle(engine, event, at, &mut ()).action,
        SessionAction::Active
    );
}

#[test]
fn join_binds_original_deadline_and_published_owner_before_any_forwarding() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let receiver = connection(2, 1);
    let request = request(&fixture, receiver, fixture.at);
    let deadline = request.deadline();
    let delayed = UnixMillis(deadline.0 - 1);
    let effects = engine.handle(
        Event::Join {
            request,
            session: wire_id(1),
        },
        delayed,
    );
    let Action::Joined(joined) = effects.action else {
        panic!("JOIN should retain its budget");
    };
    let mut session = joined.session;
    assert_eq!(session.deadline(), Some(deadline));
    let key = session.key();
    assert_eq!(key.relay(), fixture.relay());
    assert_eq!(key.share(), fixture.share());
    assert_eq!(
        key.instance(),
        published.route.registration.share_instance()
    );
    assert_eq!(key.owner(), published.owner);
    assert_eq!(key.receiver(), receiver);
    assert_eq!(key.sender(), published.owner.connection());
    assert_eq!(key.admission().incarnation(), engine.incarnation);
    assert_eq!(
        engine.observer.0.last().unwrap().decision,
        Decision::Joined(key)
    );

    let permit = forward(&engine, &mut session, receiver, delayed);
    assert_eq!(permit.protocol_version(), WIRE_VERSION);
    assert_eq!(permit.session(), key);
    assert_eq!(permit.direction(), Direction::ToSender);
    assert_eq!(permit.source(), receiver);
    assert_eq!(permit.destination(), key.sender());
    assert_eq!(
        session
            .check_forward(&engine, permit, delayed)
            .unwrap()
            .permit(),
        permit
    );
    assert_eq!(
        session
            .check_forward(&engine, permit, deadline)
            .unwrap_err(),
        ForwardRefusal::Retired(RetirementReason::AdmissionExpired)
    );
}

#[test]
fn e2e_handshake_can_flow_both_ways_before_sender_admission() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let deadline = session.deadline();
    let sender = session.key().sender();
    let receiver = session.key().receiver();
    let mut trace = SessionTrace::default();
    let premature = admitted(&session, sender);
    assert_eq!(
        session
            .handle(&engine, premature, fixture.at, &mut trace)
            .action,
        SessionAction::Refused(sessions::Refusal::ReceiverNotObserved)
    );
    let to_receiver = forward(&engine, &mut session, sender, fixture.at);
    assert_eq!(to_receiver.direction(), Direction::ToReceiver);
    assert_eq!(to_receiver.source(), sender);
    assert_eq!(to_receiver.destination(), receiver);
    assert_eq!(session.phase(), SessionPhase::AwaitingReceiver);

    let event = frame(&session, receiver);
    let to_sender = session.handle(&engine, event, fixture.at, &mut trace);
    assert!(matches!(to_sender.action, SessionAction::Forward(_)));
    assert_eq!(session.phase(), SessionPhase::AwaitingSender);
    assert_eq!(session.deadline(), deadline);
    let event = admitted(&session, sender);
    assert_eq!(
        session
            .handle(&engine, event, fixture.at, &mut trace)
            .action,
        SessionAction::Active
    );
    assert_eq!(session.deadline(), None);
    let much_later = fixture.at.saturating_add(Duration::from_hours(24));
    assert_eq!(
        session
            .handle(&engine, SessionEvent::Tick, much_later, &mut trace)
            .action,
        SessionAction::None
    );
    assert!(
        session
            .check_forward(&engine, to_receiver, much_later)
            .is_ok()
    );
    assert_eq!(
        session
            .handle(&engine, event, much_later, &mut trace)
            .action,
        SessionAction::Active
    );
    assert_eq!(trace.0.len(), 3);
    assert_eq!(trace.0[1].decision, sessions::Decision::ReceiverObserved);
    assert_eq!(trace.0[2].decision, sessions::Decision::Active);
}

#[test]
fn either_provisional_phase_expires_at_its_original_boundary() {
    let fixture = Fixture::new();
    for receiver_sent in [false, true] {
        let mut engine = fixture.engine();
        fixture.register(&mut engine);
        let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
        let deadline = session.deadline().unwrap();
        let sender = session.key().sender();
        let receiver = session.key().receiver();
        let permit = forward(&engine, &mut session, sender, fixture.at);
        if receiver_sent {
            forward(&engine, &mut session, receiver, UnixMillis(deadline.0 - 1));
            forward(&engine, &mut session, receiver, UnixMillis(deadline.0 - 1));
        }
        assert_eq!(session.deadline(), Some(deadline));
        let event = admitted(&session, sender);
        let mut trace = SessionTrace::default();
        let effects = session.handle(&engine, event, deadline, &mut trace);
        assert_eq!(
            effects.action,
            SessionAction::Discard(RetirementReason::AdmissionExpired)
        );
        assert_eq!(effects.retirement.unwrap().session, session.key());
        assert_eq!(session.phase(), SessionPhase::Retired);
        assert_eq!(session.deadline(), None);
        assert!(session.check_forward(&engine, permit, deadline).is_err());
        assert_eq!(
            trace.0[0].decision,
            sessions::Decision::Retired(RetirementReason::AdmissionExpired)
        );
        assert!(
            session
                .handle(&engine, event, deadline, &mut ())
                .retirement
                .is_none()
        );
    }
}

#[path = "session_revocation_tests.rs"]
mod revocation;

#[path = "session_authority_tests.rs"]
mod authority;

#[path = "session_restoration_tests.rs"]
mod restoration;

#[path = "flow_tests.rs"]
mod flow_tests;
