use super::Input;

#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) enum Purpose {
    Text,
    Password,
}

impl Input {
    pub const fn password(mut self) -> Self {
        self.purpose = Purpose::Password;
        self
    }
    pub(super) fn display_value(&self) -> String {
        match self.purpose {
            Purpose::Text => self.value.clone(),
            Purpose::Password => "•".repeat(self.value.chars().count()),
        }
    }
    pub(super) fn display_offset(&self, byte: usize) -> usize {
        match self.purpose {
            Purpose::Text => byte,
            Purpose::Password => self.value[..byte].chars().count() * "•".len(),
        }
    }
    pub(super) fn value_offset(&self, display: usize) -> usize {
        match self.purpose {
            Purpose::Text => display.min(self.value.len()),
            Purpose::Password => self
                .value
                .char_indices()
                .nth(display / "•".len())
                .map_or(self.value.len(), |(byte, _)| byte),
        }
    }
}
