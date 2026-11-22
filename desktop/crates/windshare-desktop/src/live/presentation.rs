use super::{
    Runtime,
    application::{Application, Presentation},
    session::QuitState,
};
use crate::{
    desktop::Desktop,
    flow_state::ShareRun,
    model::{Page, Share, ShareAge, ShareContents, ShareSource, ShareState},
    sharing::LinkDisclosure,
};
use gpui::{Context, Window};
use std::time::{Instant, SystemTime};
use windshare_ui::{Icon, Tone};

impl Desktop {
    pub fn observe_activations(application: Application, window: &Window, cx: &Context<Self>) {
        cx.spawn_in(window, async move |view, cx| {
            loop {
                application.changed().await;
                if view.update_in(cx, Self::consume_activations).is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    pub fn consume_activations(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        let pending = session.application.drain();
        for presentation in pending {
            if let Presentation::Share(id) = presentation {
                self.present_native_operation(&id, window, cx);
            }
            #[cfg(windows)]
            window.defer(cx, |window, _| {
                use raw_window_handle::{HasWindowHandle, RawWindowHandle};
                if let Ok(RawWindowHandle::Win32(handle)) =
                    HasWindowHandle::window_handle(window).map(|handle| handle.as_raw())
                {
                    windshare_activation::windows::window::present(handle.hwnd.get());
                }
            });
            #[cfg(not(windows))]
            window.activate_window();
            cx.activate(true);
        }
    }

    pub(super) fn present_native_operation(
        &mut self,
        operation_id: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<usize> {
        let Runtime::Native(session) = &mut self.runtime else {
            return None;
        };
        let model = session.controller(cx).model();
        let operation = model.operations.iter().find(|op| op.id == operation_id)?;
        session.interaction_epoch += 1;
        session.picking = false;
        #[cfg(windows)]
        {
            session.picker = None;
        }
        session.quit_state = QuitState::Running;
        session.error = None;
        windshare_ui::dismiss_prompts(cx);
        let existing = self.shares.iter().position(|share| matches!(&share.source, ShareSource::Native { operation_id: id } if id == operation_id));
        let index = if let Some(index) = existing {
            index
        } else {
            let name = operation.paths.first().map_or_else(String::new, |path| {
                path.file_name()
                    .unwrap_or(path.as_os_str())
                    .to_string_lossy()
                    .into_owned()
            });
            let count = operation.paths.len();
            self.operation_sequence += 1;
            self.shares.insert(
                0,
                Share {
                    id: self.operation_sequence,
                    source: ShareSource::Native {
                        operation_id: operation_id.into(),
                    },
                    name: if count > 1 {
                        format!("{name} + {}", count - 1)
                    } else {
                        name
                    },
                    contents: ShareContents::Selected(count),
                    size: None,
                    age: ShareAge::CreatedAt(SystemTime::now()),
                    tone: Tone::Mint,
                    symbol: Icon::Folder,
                    state: ShareState::Preparing { error: None },
                },
            );
            0
        };
        let share = &mut self.shares[index];
        super::projection::update_share(share, operation);
        let id = share.id;
        let perch = self.intro_anchors.perch.get();
        let animation = cx
            .global::<crate::preferences::ShareAnimationSettings>()
            .choice;
        // Reopening an existing operation keeps its original run, so an
        // entrance that already played is not staged a second time.
        let run = *session
            .presentations
            .entry(operation_id.into())
            .or_insert_with(|| ShareRun::new(id, Instant::now(), perch, animation));
        self.link_disclosure = LinkDisclosure::Summary;
        self.navigate(Page::Share, window, cx);
        self.present_share(run, window, cx);
        cx.notify();
        Some(id)
    }
}
