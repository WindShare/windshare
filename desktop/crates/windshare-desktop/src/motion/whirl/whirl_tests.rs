use super::{
    bloom::{self, BLOOM},
    cyclone::{MOTES, Whirl},
    flight::{self, ARRIVAL, Flight, HOME, TRAIL},
    *,
};
use std::time::Duration;

const SLOT_SIDE: f32 = 288.;
const TOLERANCE: f32 = 1e-3;

fn flight() -> Flight {
    Flight {
        from: point(380., 201.),
        to: point(585., 232.),
        from_width: 190.,
        to_width: flight::resting_width(SLOT_SIDE),
    }
}

fn close(a: Point<f32>, b: Point<f32>) -> bool {
    (a.x - b.x).abs() < TOLERANCE && (a.y - b.y).abs() < TOLERANCE
}

fn after(start: Instant, seconds: f32) -> Instant {
    start + Duration::from_secs_f32(seconds)
}

#[test]
fn flight_leaves_the_spirit_where_it_stood() {
    let flight = flight();
    let pose = flight.pose(0.);

    assert!(close(pose.center, flight.from));
    assert!((pose.width - flight.from_width).abs() < TOLERANCE);
    assert!(pose.tilt.abs() < 0.5);
}

#[test]
fn slot_takes_over_exactly_what_the_flight_last_painted() {
    let flight = flight();
    let start = Instant::now();
    // The first frame the slot paints, a moment past the handover.
    let landed = HOME + 0.01;
    let standing = Wind::Carried { since: start }
        .standing(after(start, landed))
        .expect("a carried whirl stands once every mote is home");

    // The slowest mote samples the eye a full trail in the past.
    assert!(close(flight.eye(HOME - TRAIL), flight.to));
    assert!((Flight::settling(HOME) - 1.).abs() < TOLERANCE);
    let pose = flight.pose(landed);
    assert!(close(pose.center, flight.to + point(0., standing.hover)));
    assert!((pose.width - flight.to_width).abs() < TOLERANCE);
    assert!(pose.tilt.abs() < TOLERANCE);
    assert!((standing.clock - landed).abs() < TOLERANCE);
    assert!((standing.presence - 1.).abs() < TOLERANCE);
}

#[test]
fn carried_whirl_is_absent_from_its_slot_while_in_flight() {
    let start = Instant::now();
    let wind = Wind::Carried { since: start };

    assert_eq!(wind.standing(after(start, ARRIVAL)), None);
}

#[test]
fn rooted_whirl_rises_with_its_spirit() {
    let start = Instant::now();
    let wind = Wind::Rooted { since: start };
    let sprouting = wind.standing(start).expect("a rooted whirl always stands");
    let risen = wind
        .standing(after(start, HOLD))
        .expect("a rooted whirl always stands");

    assert!(sprouting.presence.abs() < TOLERANCE);
    assert!((risen.presence - 1.).abs() < TOLERANCE);
}

#[test]
fn standing_motes_stay_inside_their_slot() {
    let whirl = Whirl::resting(flight::resting_width(SLOT_SIDE), SLOT_SIDE);

    assert!(whirl.rim < SLOT_SIDE / 2.);
    for step in 0_u8..40 {
        let clock = f32::from(step) * 0.37;
        for index in 0..MOTES {
            let mote = whirl.mote(index, clock, 0.);
            assert!(mote.offset.x.hypot(mote.offset.y) <= whirl.rim + TOLERANCE);
            assert!(mote.side >= 0.);
        }
    }
}

#[test]
fn bloom_starts_in_the_eye_and_ends_exactly_on_the_code() {
    let reach = 150.;
    for (x, y) in [(-96., -96.), (12., -40.), (0., 3.), (106., 106.)] {
        let rest = point(x, y);
        let seed = bloom::petal(rest, reach, 0.);
        let settled = bloom::petal(rest, reach, 1.);

        assert!(seed.scale.abs() < TOLERANCE);
        assert!(close(seed.offset, point(0., 0.)));
        // Bit-exact, not approximate: the last animated frame is the static code.
        assert_eq!(settled.offset, rest);
        assert_eq!(settled.scale.to_bits(), 1_f32.to_bits());
        assert_eq!(settled.turn.to_bits(), 0_f32.to_bits());
    }
}

#[test]
fn eye_has_swallowed_whirl_and_spirit_before_the_code_settles() {
    let late = 0.9;

    assert!((bloom::collapse(late) - 1.).abs() < TOLERANCE);
    assert!(bloom::leave(late).scale.abs() < TOLERANCE);
    assert!((bloom::ink(1.) - 1.).abs() < TOLERANCE);
    assert!(bloom::halo(BLOOM + bloom::HALO).is_none());
}
