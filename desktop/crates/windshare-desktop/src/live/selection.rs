use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
};
#[cfg(not(windows))]
use gpui::PathPromptOptions;
use gpui::{Context, Window};
use std::path::PathBuf;
use windshare_activation::{Activation, EntrySource, ShareIntent};

impl Desktop {
    pub fn choose_native(&mut self, folders: bool, window: &Window, cx: &Context<Self>) {
        if self.runtime.selection_disabled() {
            return;
        }
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        if session.picking {
            return;
        }
        session.picking = true;
        session.error = None;
        session.interaction_epoch += 1;
        let epoch = session.interaction_epoch;
        #[cfg(not(windows))]
        let selection = cx.prompt_for_paths(PathPromptOptions {
            files: !folders,
            directories: folders,
            multiple: true,
            prompt: Some(
                language(cx)
                    .text(if folders {
                        Text::ChooseFolder
                    } else {
                        Text::ChooseFiles
                    })
                    .into(),
            ),
        });
        #[cfg(windows)]
        let selection = {
            use raw_window_handle::{HasWindowHandle, RawWindowHandle};
            let owner = match HasWindowHandle::window_handle(window).map(|handle| handle.as_raw()) {
                Ok(RawWindowHandle::Win32(handle)) => handle.hwnd.get(),
                _ => 0,
            };
            let title = language(cx)
                .text(if folders {
                    Text::ChooseFolder
                } else {
                    Text::ChooseFiles
                })
                .to_owned();
            match windshare_activation::windows::picker::Picker::start(owner, folders, title) {
                Ok((picker, receiver)) => {
                    session.picker = Some(picker);
                    receiver
                }
                Err(error) => {
                    session.picking = false;
                    session.error = Some(error.to_string());
                    return;
                }
            }
        };
        let generation = session.application.generation();
        tracing::info!(event = "desktop.selection_opened", folders, epoch);
        cx.spawn_in(window, async move |view, cx| {
            #[cfg(not(windows))]
            let result = selection
                .await
                .map_err(|error| error.to_string())
                .and_then(|value| value.map_err(|error| error.to_string()));
            #[cfg(windows)]
            let result = selection
                .recv()
                .await
                .map_err(|error| error.to_string())
                .and_then(std::convert::identity);
            let _ = view.update_in(cx, |this, window, cx| {
                let Runtime::Native(session) = &mut this.runtime else {
                    return;
                };
                if session.interaction_epoch != epoch
                    || session.application.generation() != generation
                {
                    return;
                }
                session.picking = false;
                match result {
                    Ok(Some(paths)) if !paths.is_empty() => {
                        this.submit_native_share(paths, EntrySource::Picker, window, cx);
                    }
                    Ok(_) => tracing::info!(event = "desktop.selection_cancelled"),

                    Err(error) => session.error = Some(error),
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub fn submit_native_share(
        &mut self,
        paths: Vec<PathBuf>,
        source: EntrySource,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if paths.is_empty() {
            return;
        }
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        let intent = ShareIntent::new(source, paths);
        if let Err(error) = session
            .application
            .submit(Activation::ShareSelection(intent))
        {
            session.error = Some(error);
            cx.notify();
            return;
        }
        self.consume_activations(window, cx);
    }

    pub fn repeat_native_share(
        &mut self,
        id: usize,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<usize> {
        if self.runtime.selection_disabled() {
            return None;
        }
        let Runtime::Native(session) = &mut self.runtime else {
            return None;
        };
        let share = self.shares.iter().find(|share| share.id == id)?;
        let crate::model::ShareSource::Native { operation_id } = &share.source else {
            return None;
        };
        match session.controller.repeat(operation_id) {
            Ok(operation_id) => self.present_native_operation(&operation_id, window, cx),
            Err(error) => {
                session.error = Some(error);
                cx.notify();
                None
            }
        }
    }
}
