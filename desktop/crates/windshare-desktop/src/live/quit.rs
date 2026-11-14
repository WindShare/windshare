use super::{Runtime, session::QuitState};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
};
use gpui::{Context, PromptButton, PromptLevel, Window};

const CONFIRM_QUIT: usize = 1;

impl Desktop {
    pub fn request_quit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &mut self.runtime else {
            self.quit(cx);
            return;
        };
        if session.quit_state != QuitState::Running {
            return;
        }
        // Read application ownership, including queued selections and setup,
        // rather than the currently visible page or a possibly stale projection.
        if !session.controller.model().has_unfinished_work() {
            self.quit(cx);
            return;
        }
        session.quit_state = QuitState::Confirming;
        let locale = language(cx);
        let answer = window.prompt(
            PromptLevel::Warning,
            locale.text(Text::QuitWithShares),
            None,
            &[
                PromptButton::Cancel(locale.text(Text::KeepRunning).into()),
                PromptButton::Other(locale.text(Text::QuitAndStopShares).into()),
            ],
            cx,
        );
        cx.spawn(async move |view, cx| {
            let confirmed = answer.await == Ok(CONFIRM_QUIT);
            let _ = view.update(cx, |this, cx| {
                tracing::info!(event = "desktop.quit_decision", confirmed);
                if confirmed {
                    this.quit(cx);
                } else if let Runtime::Native(session) = &mut this.runtime {
                    session.quit_state = QuitState::Running;
                    cx.notify();
                }
            });
        })
        .detach();
    }
}
