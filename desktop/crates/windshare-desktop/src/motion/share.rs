/// The entrance a share captures when it starts. Changing preferences affects
/// the next share, never a presentation already in progress.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ShareAnimation {
    #[default]
    Whirl,
    Paper,
}

impl ShareAnimation {
    pub const ALL: [Self; 2] = [Self::Whirl, Self::Paper];

    pub const fn duration(self) -> f32 {
        match self {
            Self::Whirl => super::whirl::HOLD,
            Self::Paper => super::paper::DURATION,
        }
    }
}
