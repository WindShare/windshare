use std::f32::consts::PI;

pub fn smoothstep(from: f32, to: f32, value: f32) -> f32 {
    let x = ((value - from) / (to - from)).clamp(0., 1.);
    x * x * 2.0_f32.mul_add(-x, 3.)
}

pub fn ease_in_cubic(x: f32) -> f32 {
    x.clamp(0., 1.).powi(3)
}

pub fn ease_out_cubic(x: f32) -> f32 {
    1. - (1. - x.clamp(0., 1.)).powi(3)
}

pub fn ease_out_quint(x: f32) -> f32 {
    1. - (1. - x.clamp(0., 1.)).powi(5)
}

pub fn ease_in_out_cubic(x: f32) -> f32 {
    let x = x.clamp(0., 1.);
    if x < 0.5 {
        4. * x.powi(3)
    } else {
        1. - (-2.0_f32).mul_add(x, 2.).powi(3) / 2.
    }
}

/// Rises from rest to one and returns to rest, without a velocity jump at either end.
pub fn swell(x: f32) -> f32 {
    (PI * x.clamp(0., 1.)).sin().powi(2)
}
