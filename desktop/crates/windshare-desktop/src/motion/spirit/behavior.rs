use std::{
    f32::consts::TAU,
    time::{Duration, Instant},
};

pub const NOMINAL_SIZE: f32 = 156.;
pub const SMILE_DURATION: Duration = Duration::from_millis(460);
const ATTENTION_DURATION: Duration = Duration::from_millis(420);
const FLOAT_CYCLE_SECONDS: f32 = 3.4;
const FLOAT_AMPLITUDE: f32 = 4.;
const FOLLOW_RESPONSE: f32 = 22.;
const REST_RESPONSE: f32 = 10.;
const HORIZONTAL_TRAVEL_RATIO: f32 = 0.09;
const VERTICAL_TRAVEL_RATIO: f32 = 0.06;
const MAX_TILT_DEGREES: f32 = 6.;
const TAP_LIFT_SPEED: f32 = -230.;
const TAP_TILT_SPEED: f32 = 85.;

#[derive(Clone, Copy, Default)]
pub struct Pose {
    pub x: f32,
    pub y: f32,
    pub tilt: f32,
}

impl Pose {
    pub fn for_pointer(horizontal: f32, vertical: f32) -> Self {
        Self {
            x: horizontal.clamp(-1., 1.) * NOMINAL_SIZE * HORIZONTAL_TRAVEL_RATIO,
            y: vertical.clamp(-1., 1.) * NOMINAL_SIZE * VERTICAL_TRAVEL_RATIO,
            tilt: horizontal.clamp(-1., 1.) * MAX_TILT_DEGREES,
        }
    }
}

#[derive(Default)]
enum Drive {
    #[default]
    Rest,
    Following {
        target: Pose,
        until: Instant,
    },
}

#[derive(Default)]
pub struct Behavior {
    pose: Pose,
    velocity: Pose,
    drive: Drive,
    float_phase: f32,
    smile_until: Option<Instant>,
}

impl Behavior {
    pub const fn following(&self) -> bool {
        matches!(self.drive, Drive::Following { .. })
    }

    pub fn follow(&mut self, target: Pose, now: Instant) {
        self.drive = Drive::Following {
            target,
            until: now + ATTENTION_DURATION,
        };
    }

    pub const fn release(&mut self) -> bool {
        let following = self.following();
        self.drive = Drive::Rest;
        following
    }

    pub fn tap(&mut self, now: Instant, animated: bool) {
        self.release();
        self.smile_until = Some(now + SMILE_DURATION);
        if animated {
            // Match the web spring, including displacement compensation: repeated
            // taps must not accumulate momentum and push the character out of reach.
            self.velocity = Pose {
                x: 0.,
                y: (-REST_RESPONSE).mul_add(self.pose.y, TAP_LIFT_SPEED),
                tilt: (-REST_RESPONSE).mul_add(self.pose.tilt, TAP_TILT_SPEED),
            };
        }
    }

    pub fn smiling(&self, now: Instant) -> bool {
        self.smile_until.is_some_and(|until| now < until)
    }

    pub fn advance(&mut self, elapsed: Duration, now: Instant) -> Pose {
        if matches!(self.drive, Drive::Following { until, .. } if now >= until) {
            self.release();
        }
        let (target, response) = match self.drive {
            Drive::Rest => (Pose::default(), REST_RESPONSE),
            Drive::Following { target, .. } => (target, FOLLOW_RESPONSE),
        };
        let seconds = elapsed.as_secs_f32();
        (self.pose.x, self.velocity.x) =
            advance_axis(self.pose.x, self.velocity.x, target.x, seconds, response);
        (self.pose.y, self.velocity.y) =
            advance_axis(self.pose.y, self.velocity.y, target.y, seconds, response);
        (self.pose.tilt, self.velocity.tilt) = advance_axis(
            self.pose.tilt,
            self.velocity.tilt,
            target.tilt,
            seconds,
            response,
        );
        self.float_phase = (self.float_phase + seconds / FLOAT_CYCLE_SECONDS).fract();
        Pose {
            y: (self.float_phase * TAU)
                .sin()
                .mul_add(FLOAT_AMPLITUDE, self.pose.y),
            ..self.pose
        }
    }

    pub fn reset(&mut self) {
        *self = Self::default();
    }
}

fn advance_axis(
    position: f32,
    velocity: f32,
    target: f32,
    seconds: f32,
    response: f32,
) -> (f32, f32) {
    // The analytic critically damped solution preserves the response at any
    // refresh rate; pointer events change the target without restarting time.
    let displacement = position - target;
    let decay = (-response * seconds).exp();
    let momentum = response.mul_add(displacement, velocity);
    (
        momentum
            .mul_add(seconds, displacement)
            .mul_add(decay, target),
        (-response * momentum).mul_add(seconds, velocity) * decay,
    )
}
