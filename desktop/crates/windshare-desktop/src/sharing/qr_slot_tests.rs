use super::*;
use crate::motion::whirl::flight::HOME;

const WAITING: Signal = Signal {
    code: false,
    pending: true,
    still: false,
    arrival: Arrival::Whirl(None),
};
const READY: Signal = Signal {
    code: true,
    ..WAITING
};
const CLOSED: Signal = Signal {
    pending: false,
    ..WAITING
};

fn after(start: Instant, seconds: f32) -> Instant {
    start + Duration::from_secs_f32(seconds)
}

#[test]
fn code_that_is_already_there_needs_no_ceremony() {
    let now = Instant::now();

    assert_eq!(Phase::enter(READY, now), Phase::Settled { bloomed: None });
}

#[test]
fn waiting_slot_blooms_when_the_code_arrives_then_settles() {
    let start = Instant::now();
    let wind = Wind::Rooted { since: start };
    let waiting = Phase::enter(WAITING, start);
    assert_eq!(waiting, Phase::Gathering { wind });
    assert_eq!(waiting.advance(WAITING, after(start, 3.)), waiting);

    let opened = after(start, 3.5);
    let blooming = waiting.advance(READY, opened);
    assert_eq!(
        blooming,
        Phase::Blooming {
            wind,
            since: opened
        }
    );
    assert_eq!(blooming.advance(READY, after(opened, BLOOM / 2.)), blooming);
    assert_eq!(
        blooming.advance(READY, after(opened, BLOOM + 0.1)),
        Phase::Settled {
            bloomed: Some(after(opened, BLOOM))
        }
    );
}

#[test]
fn code_waits_for_a_whirl_that_is_still_in_flight() {
    let start = Instant::now();
    let wind = Wind::Carried { since: start };
    let run = Signal {
        arrival: Arrival::Whirl(Some(wind)),
        ..READY
    };
    let waiting = Phase::Gathering { wind };

    assert_eq!(waiting.advance(run, after(start, HOME / 2.)), waiting);
    let landed = after(start, HOME + 0.1);
    assert_eq!(
        waiting.advance(run, landed),
        Phase::Blooming {
            wind,
            since: landed
        }
    );
}

#[test]
fn reduced_motion_shows_the_code_without_a_bloom() {
    let start = Instant::now();
    let still = Signal {
        still: true,
        ..READY
    };

    assert_eq!(
        Phase::enter(WAITING, start).advance(still, after(start, 3.)),
        Phase::Settled { bloomed: None }
    );
}

#[test]
fn share_that_ends_without_a_link_stops_waiting() {
    let start = Instant::now();
    let later = after(start, 3.);

    assert_eq!(Phase::enter(CLOSED, start), Phase::Unavailable);
    assert_eq!(
        Phase::enter(WAITING, start).advance(CLOSED, later),
        Phase::Unavailable
    );
    assert_eq!(
        Phase::Unavailable.advance(READY, later),
        Phase::Settled { bloomed: None }
    );
}

#[test]
fn paper_waits_for_a_late_link_then_fades_and_settles() {
    let start = Instant::now();
    let pending = Signal {
        arrival: Arrival::Fade,
        ..WAITING
    };
    let ready = Signal {
        code: true,
        ..pending
    };
    let waiting = Phase::enter(pending, start);
    assert_eq!(waiting, Phase::Waiting);
    let opened = after(start, 4.);
    assert_eq!(waiting.advance(pending, opened), waiting);
    let fading = waiting.advance(ready, opened);
    assert_eq!(fading, Phase::Fading { since: opened });
    assert_eq!(fading.advance(ready, after(opened, FADE / 2.)), fading);
    assert_eq!(
        fading.advance(ready, after(opened, FADE)),
        Phase::Settled { bloomed: None }
    );
    assert_eq!(waiting.advance(CLOSED, opened), Phase::Unavailable);
    assert_eq!(
        fading.advance(
            Signal {
                still: true,
                ..ready
            },
            opened
        ),
        Phase::Settled { bloomed: None }
    );
}
