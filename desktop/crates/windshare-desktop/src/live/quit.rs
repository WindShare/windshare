use super::{Runtime, session::QuitState};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
};
use gpui::{Context, PromptButton, PromptLevel, Window};

const CONFIRM_QUIT: usize = 1;

impl Desktop {
    fn quit(&mut self, generation: u64, window: &mut Window, cx: &mut Context<Self>) {
        if let Runtime::Native(session) = &mut self.runtime {
            if !session.application.commit_shutdown(generation) {
                session.quit_state = QuitState::Running;
                return;
            }
            session.quit_state = QuitState::Exiting;
            session
                .host
                .update(cx, |host, cx| host.set_exiting(true, cx));
            // Seal command admission before the platform processes the quit request.
        }
        tracing::info!(
            event = "desktop.window_close_requested",
            session = std::process::id()
        );
        // Close while native messages can still run: Windows queues its window
        // destruction. LastWindowClosed then ends GPUI; the application joins the host.
        window.remove_window();
    }

    pub fn request_quit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &mut self.runtime else {
            self.quit(0, window, cx);
            return;
        };
        if session.quit_state != QuitState::Running {
            return;
        }
        let generation = session.application.generation();
        // Read application ownership, including queued selections and setup,
        // rather than the currently visible page or a possibly stale projection.
        if !session.controller(cx).model().has_unfinished_work() {
            self.quit(generation, window, cx);
            return;
        }
        session.quit_state = QuitState::Confirming;
        let epoch = session.interaction_epoch;
        tracing::info!(
            event = "desktop.quit_confirmation_opened",
            session = std::process::id()
        );
        let locale = language(cx);
        let answer = window.prompt(
            PromptLevel::Warning,
            locale.text(Text::QuitWithShares),
            Some(locale.text(Text::QuitWithSharesDetail)),
            &[
                PromptButton::Cancel(locale.text(Text::KeepRunning).into()),
                PromptButton::Other(locale.text(Text::QuitAndStopShares).into()),
            ],
            cx,
        );
        cx.spawn_in(window, async move |view, cx| {
            let confirmed = answer.await == Ok(CONFIRM_QUIT);
            let _ = view.update_in(cx, |this, window, cx| {
                if !matches!(&this.runtime, Runtime::Native(session) if session.interaction_epoch == epoch && session.quit_state == QuitState::Confirming) {
                    return;
                }
                tracing::info!(
                    event = "desktop.quit_decision",
                    session = std::process::id(),
                    confirmed
                );
                if confirmed {
                    this.quit(generation, window, cx);
                } else if let Runtime::Native(session) = &mut this.runtime {
                    session.quit_state = QuitState::Running;
                    cx.notify();
                }
            });
        })
        .detach();
    }
}
