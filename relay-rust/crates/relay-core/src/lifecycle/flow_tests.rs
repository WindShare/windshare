use super::*;
use crate::flow::{
    self, ConfigurationError, ConnectionLedger, Counts, Feedback, FlowKey, Limits, ReceivedFrame,
    Refusal as FlowRefusal, Usage,
};
use crate::sessions::Retirement;

const MAX_FRAME: u64 = OpaqueHeader::MAX_ENCODED_BYTES as u64;
const SMALL_FRAME: u64 = OpaqueHeader::MIN_ENCODED_BYTES as u64;
const SETTLEMENT_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Debug, Default)]
struct FlowTrace(Vec<flow::Observation>);

impl flow::Observer for FlowTrace {
    fn observe(&mut self, observation: flow::Observation) {
        self.0.push(observation);
    }
}

fn counts(frames: u64) -> Counts {
    Counts::new(frames, frames * MAX_FRAME)
}

fn limits() -> Limits {
    Limits::new(counts(8), counts(2), counts(1), SETTLEMENT_TIMEOUT).unwrap()
}

fn ledger<const N: usize>(
    fixture: &Fixture,
    destination: ConnectionRef,
) -> ConnectionLedger<N, FlowTrace> {
    ConnectionLedger::new(fixture.relay(), destination, limits(), FlowTrace::default()).unwrap()
}

fn open<const N: usize>(
    ledger: &mut ConnectionLedger<N, FlowTrace>,
    engine: &ShareLifecycle<Recorder>,
    session: &Session,
    direction: Direction,
    at: UnixMillis,
) -> FlowKey {
    let route = session.check_route(engine, direction, at).unwrap();
    let grant = ledger.open(&route, at).unwrap();
    assert_eq!(grant.credit, counts(1));
    grant.flow
}

fn header(session: &Session, ciphertext: u32) -> OpaqueHeader {
    let mut bytes = Vec::new();
    bytes.extend_from_slice(b"WS2O");
    bytes.extend_from_slice(&[WIRE_VERSION, 0, 0, 0]);
    bytes.extend_from_slice(session.key().wire_id().as_bytes());
    bytes.extend_from_slice(&ciphertext.to_be_bytes());
    bytes.resize(bytes.len() + ciphertext as usize, 0xff);
    OpaqueHeader::decode(&bytes).unwrap()
}

fn receive<const N: usize>(
    ledger: &mut ConnectionLedger<N, FlowTrace>,
    engine: &ShareLifecycle<Recorder>,
    session: &mut Session,
    flow: FlowKey,
    ciphertext: u32,
    at: UnixMillis,
) -> ReceivedFrame {
    let header = header(session, ciphertext);
    let event = SessionEvent::Frame {
        source: flow.permit().source(),
        header,
    };
    let effects = session.handle(engine, event, at, &mut ());
    assert_eq!(effects.action, SessionAction::Forward(flow.permit()));
    let route = session.check_forward(engine, flow.permit(), at).unwrap();
    ledger.receive(flow, &route, header, at).unwrap()
}

fn submit<const N: usize>(
    ledger: &mut ConnectionLedger<N, FlowTrace>,
    engine: &ShareLifecycle<Recorder>,
    session: &Session,
    receipt: &ReceivedFrame,
    at: UnixMillis,
) {
    let route = session
        .check_forward(engine, receipt.flow().permit(), at)
        .unwrap();
    ledger.submit(&route, receipt, at).unwrap();
}

fn cancel(engine: &ShareLifecycle<Recorder>, session: &mut Session, at: UnixMillis) -> Retirement {
    let event = SessionEvent::End {
        session: session.key(),
        participant: session.key().receiver(),
        reason: EndReason::Cancelled,
    };
    session
        .handle(engine, event, at, &mut ())
        .retirement
        .unwrap()
}

#[test]
fn startup_credit_precedes_frames_in_both_directions_without_advancing_admission() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut sender = ledger::<2>(&fixture, session.key().sender());
    let mut receiver = ledger::<2>(&fixture, session.key().receiver());
    let up = open(
        &mut sender,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    let down = open(
        &mut receiver,
        &engine,
        &session,
        Direction::ToReceiver,
        fixture.at,
    );
    assert_eq!(session.phase(), SessionPhase::AwaitingReceiver);
    assert_eq!(up.session(), down.session());
    assert_ne!(up.direction(), down.direction());
    let premature = admitted(&session, session.key().sender());
    assert_eq!(
        session
            .handle(&engine, premature, fixture.at, &mut ())
            .action,
        SessionAction::Refused(sessions::Refusal::ReceiverNotObserved)
    );
    for (ledger, flow) in [(&mut sender, up), (&mut receiver, down)] {
        let receipt = receive(ledger, &engine, &mut session, flow, 1, fixture.at);
        submit(ledger, &engine, &session, &receipt, fixture.at);
        assert_eq!(ledger.usage().submitted, Counts::new(1, SMALL_FRAME));
    }
    let event = admitted(&session, session.key().sender());
    assert_eq!(
        session.handle(&engine, event, fixture.at, &mut ()).action,
        SessionAction::Active
    );
    assert_eq!(sender.deadline(), None);
    let later = fixture.at.saturating_add(Duration::from_hours(24));
    assert_eq!(sender.tick(later), None);
}

#[test]
fn busy_sessions_cannot_spend_new_session_startup_capacity() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    let published = fixture.register(&mut engine);
    let first = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let second = join(&fixture, &mut engine, connection(3, 1), 2, fixture.at);
    let third = join(&fixture, &mut engine, connection(4, 1), 3, fixture.at);
    let mut ledger = ledger::<3>(&fixture, published.owner.connection());
    let flow = open(
        &mut ledger,
        &engine,
        &first,
        Direction::ToSender,
        fixture.at,
    );
    let route = first
        .check_forward(&engine, flow.permit(), fixture.at)
        .unwrap();
    let grant = ledger.grant(flow, &route, counts(6), fixture.at).unwrap();
    assert_eq!(grant.credit, counts(6));
    assert_eq!(ledger.regular_used(), counts(6));
    for excess in [Counts::new(1, 0), Counts::new(0, 1)] {
        assert_eq!(
            ledger.grant(flow, &route, excess, fixture.at),
            Err(FlowRefusal::DeliveryCapacity)
        );
    }
    let second_flow = open(
        &mut ledger,
        &engine,
        &second,
        Direction::ToSender,
        fixture.at,
    );
    assert_ne!(flow, second_flow);
    assert_eq!(ledger.usage().total(), limits().total());
    let third_route = third
        .check_route(&engine, Direction::ToSender, fixture.at)
        .unwrap();
    assert_eq!(
        ledger.open(&third_route, fixture.at),
        Err(FlowRefusal::StartupCapacity)
    );
    assert_eq!(ledger.usage().total(), counts(8));
}

#[test]
fn received_and_submitted_payloads_do_not_refund_credit_and_fifo_is_enforced() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger::<2>(&fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    {
        let route = session
            .check_forward(&engine, flow.permit(), fixture.at)
            .unwrap();
        let grant = ledger.grant(flow, &route, counts(1), fixture.at).unwrap();
        assert_eq!(grant.credit, counts(1));
    }
    let first = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let second = receive(&mut ledger, &engine, &mut session, flow, 2, fixture.at);
    assert_eq!(first.encoded_bytes(), SMALL_FRAME);
    assert_eq!(ledger.usage().received, Counts::new(2, SMALL_FRAME * 2 + 1));
    let route = session
        .check_forward(&engine, flow.permit(), fixture.at)
        .unwrap();
    let before = ledger.usage();
    assert_eq!(
        ledger.submit(&route, &second, fixture.at),
        Err(FlowRefusal::SubmissionOrder)
    );
    assert_eq!(ledger.usage(), before);
    ledger.submit(&route, &first, fixture.at).unwrap();
    assert_eq!(
        ledger.submit(&route, &first, fixture.at),
        Err(FlowRefusal::SubmissionOrder)
    );
    ledger.submit(&route, &second, fixture.at).unwrap();
    assert_eq!(ledger.usage().total(), counts(2));
    assert_eq!(ledger.usage().received, Counts::ZERO);
    assert_eq!(
        ledger.receive(flow, &route, header(&session, 1), fixture.at),
        Err(FlowRefusal::InsufficientCredit)
    );
    let consumed = Counts::new(2, SMALL_FRAME * 2 + 1);
    assert_eq!(
        ledger.consume(flow, flow.destination(), consumed, fixture.at),
        Ok(Feedback::Released(consumed))
    );
    assert_eq!(ledger.usage().submitted, Counts::ZERO);
    assert_eq!(
        ledger.startup_used(),
        Counts::new(0, MAX_FRAME - consumed.encoded_bytes)
    );
    assert_eq!(ledger.regular_used(), Counts::new(0, MAX_FRAME));
}

#[test]
fn frame_and_byte_grants_exhaust_independently_and_replenishment_never_uses_startup() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger::<2>(&fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    let frame = receive(
        &mut ledger,
        &engine,
        &mut session,
        flow,
        sessions::MAX_OPAQUE_CIPHERTEXT_BYTES,
        fixture.at,
    );
    submit(&mut ledger, &engine, &session, &frame, fixture.at);
    assert_eq!(
        ledger.consume(flow, flow.destination(), counts(1), fixture.at),
        Ok(Feedback::Released(counts(1)))
    );
    assert_eq!(ledger.startup_used(), Counts::ZERO);
    let route = session
        .check_forward(&engine, flow.permit(), fixture.at)
        .unwrap();
    let grant = ledger
        .grant(flow, &route, Counts::new(6, 0), fixture.at)
        .unwrap();
    assert_eq!(grant.credit.encoded_bytes, 0);
    assert_eq!(
        ledger.receive(flow, &route, header(&session, 1), fixture.at),
        Err(FlowRefusal::InsufficientCredit)
    );
    let grant = ledger
        .grant(flow, &route, Counts::new(0, MAX_FRAME * 6), fixture.at)
        .unwrap();
    assert_eq!(grant.credit.frames, 0);
    assert_eq!(
        ledger.grant(flow, &route, counts(1), fixture.at),
        Err(FlowRefusal::DeliveryCapacity)
    );
    assert_eq!(ledger.startup_used(), Counts::ZERO);
}

#[test]
fn malformed_consumption_cannot_create_capacity() {
    let fixture = Fixture::new();
    let mut engine = fixture.engine();
    fixture.register(&mut engine);
    let mut session = join(&fixture, &mut engine, connection(2, 1), 1, fixture.at);
    let mut ledger = ledger::<1>(&fixture, session.key().sender());
    let flow = open(
        &mut ledger,
        &engine,
        &session,
        Direction::ToSender,
        fixture.at,
    );
    let receipt = receive(&mut ledger, &engine, &mut session, flow, 1, fixture.at);
    let total = Counts::new(1, SMALL_FRAME);
    assert_eq!(
        ledger.consume(flow, flow.destination(), total, fixture.at),
        Err(FlowRefusal::InvalidConsumption)
    );
    submit(&mut ledger, &engine, &session, &receipt, fixture.at);
    let before = ledger.usage();
    for invalid in [
        Counts::new(2, SMALL_FRAME),
        Counts::new(1, SMALL_FRAME + 1),
        Counts::new(0, 1),
        Counts::new(1, 0),
        Counts::new(1, 1),
        Counts::new(1, SMALL_FRAME - 1),
    ] {
        assert_eq!(
            ledger.consume(flow, flow.destination(), invalid, fixture.at),
            Err(FlowRefusal::InvalidConsumption)
        );
        assert_eq!(ledger.usage(), before);
    }
    assert_eq!(
        ledger.consume(flow, flow.destination(), total, fixture.at),
        Ok(Feedback::Released(total))
    );
    for duplicate in [total, Counts::ZERO] {
        assert_eq!(
            ledger.consume(flow, flow.destination(), duplicate, fixture.at),
            Ok(Feedback::Unchanged)
        );
    }
    assert_eq!(
        ledger.consume(
            flow,
            flow.destination(),
            Counts::new(0, SMALL_FRAME + 1),
            fixture.at
        ),
        Err(FlowRefusal::InvalidConsumption)
    );
}

#[path = "flow_retirement_tests.rs"]
mod retirement;

#[path = "flow_authority_tests.rs"]
mod authority;

#[path = "flow_capacity_tests.rs"]
mod capacity;
