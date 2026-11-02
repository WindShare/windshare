use gpui::{prelude::*, *};
use std::{
    f32::consts::{PI, TAU},
    time::Duration,
};
use windshare_ui::*;

const SCENE_WIDTH: f32 = 340.;
const SCENE_HEIGHT: f32 = 176.;
const SPIRIT_SIZE: f32 = 156.;
const FLOAT_CYCLE_MS: u64 = 3400;
const FLIGHT_CYCLE_MS: u64 = 1500;
const PAPERS: [&str; 3] = ["brand/paper.svg", "brand/photo.svg", "brand/folder.svg"];

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum SpiritMood {
    Idle,
    Sending,
    Receiving,
    Happy,
}

pub fn spirit_scene(mood: SpiritMood, tone: Tone, still: bool) -> Div {
    scaled_spirit(mood, tone, 1., still)
}

pub fn scaled_spirit(mood: SpiritMood, tone: Tone, scale: f32, still: bool) -> Div {
    let active = matches!(mood, SpiritMood::Sending | SpiritMood::Receiving);
    let unit = |value| px(value * scale);
    let spirit = div()
        .absolute()
        .left(unit(92.))
        .top(unit(4.))
        .size(unit(SPIRIT_SIZE))
        .child(
            img(if mood == SpiritMood::Happy {
                "brand/spirit-happy.svg"
            } else {
                "brand/spirit.svg"
            })
            .size_full(),
        );
    let spirit = if still {
        spirit.into_any_element()
    } else {
        spirit
            .with_animation(
                ("spirit-float", mood as usize),
                Animation::new(Duration::from_millis(FLOAT_CYCLE_MS)).repeat(),
                move |el, delta| el.top(px((4. + (delta * TAU).sin() * 4.) * scale)),
            )
            .into_any_element()
    };
    // Open wind lines connect the character to the files without isolating it in a badge.
    let mut scene = div()
        .relative()
        .w(unit(SCENE_WIDTH))
        .h(unit(SCENE_HEIGHT))
        .flex_shrink_0()
        .child(
            vector_sprite(
                "brand/breeze.svg",
                size(px(SCENE_WIDTH), px(SCENE_HEIGHT)),
                point(unit(SCENE_WIDTH / 2.), unit(SCENE_HEIGHT / 2.)),
                scale,
            )
            .text_color(tone.accent()),
        );
    for (index, asset) in PAPERS.into_iter().enumerate() {
        let paper = div()
            .absolute()
            .w(unit(36.))
            .h(unit(41.))
            .child(img(asset).size_full());
        let element = if active && !still {
            paper
                .with_animation(
                    ("flying-file", index + mood as usize * 10),
                    Animation::new(Duration::from_millis(FLIGHT_CYCLE_MS)).repeat(),
                    move |el, delta| {
                        let phase = (delta + index as f32 / 3.) % 1.;
                        let travel = if mood == SpiritMood::Receiving {
                            1. - phase
                        } else {
                            phase
                        };
                        el.left(px((12. + travel * 282.) * scale))
                            .top(px((96. - (phase * PI).sin() * 73.) * scale))
                            .opacity((phase * PI).sin() * 0.95)
                    },
                )
                .into_any_element()
        } else {
            let (x, y) = [(31., 63.), (268., 26.), (265., 123.)][index];
            let paper = paper.left(unit(x)).top(unit(y));
            if still {
                paper.into_any_element()
            } else {
                paper
                    .with_animation(
                        ("paper-drift", index + mood as usize * 10),
                        Animation::new(Duration::from_millis(FLOAT_CYCLE_MS + index as u64 * 450))
                            .repeat(),
                        move |el, delta| {
                            el.top(px((y + (delta * TAU + index as f32).sin() * 3.) * scale))
                        },
                    )
                    .into_any_element()
            }
        };
        scene = scene.child(element);
    }
    // File paths pass behind the character so its face remains unobstructed during preparation.
    scene.child(spirit)
}

pub fn reassurance(symbol: Icon, text: &'static str) -> Div {
    row()
        .gap(px(7.))
        .child(icon(symbol, 16.).text_color(muted()))
        .child(caption(text))
}

pub fn reveal(content: Div, id: impl Into<ElementId>, still: bool) -> AnyElement {
    content
        .with_animation(
            id,
            Animation::new(Duration::from_millis(if still { 1 } else { 300 }))
                .with_easing(ease_out_quint()),
            |el, delta| el.opacity(delta).relative().top(px((1. - delta) * 7.)),
        )
        .into_any_element()
}
