use super::Runtime;
use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareReadiness, ShareRun},
    localization::{Text, language},
    model::{Page, Share, ShareAge, ShareContents, ShareSource, ShareState},
    sharing::LinkDisclosure,
};
use gpui::{Context, PathPromptOptions, Window};
use std::{path::PathBuf, time::Instant};
use windshare_ui::{Icon, Tone};

impl Desktop {
    pub fn choose_native(&mut self, folders: bool, window: &Window, cx: &Context<Self>) {
        if self.runtime.selection_disabled() {
            return;
        }
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        session.picking = true;
        session.error = None;
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
        tracing::info!(event = "desktop.selection_opened", folders);
        cx.spawn_in(window, async move |view, cx| {
            let result = selection.await;
            let _ = view.update_in(cx, |this, window, cx| {
                let Runtime::Native(session) = &mut this.runtime else {
                    return;
                };
                session.picking = false;
                match result {
                    Ok(Ok(Some(paths))) if !paths.is_empty() => {
                        this.create_native_share(
                            paths,
                            if folders { Icon::Folder } else { Icon::File },
                            window,
                            cx,
                        );
                    }
                    Ok(Ok(_)) => tracing::info!(event = "desktop.selection_cancelled"),
                    Ok(Err(error)) => {
                        tracing::error!(event = "desktop.selection_failed", %error);
                        session.error = Some(error.to_string());
                    }
                    Err(error) => session.error = Some(error.to_string()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub fn create_native_share(
        &mut self,
        paths: Vec<PathBuf>,
        symbol: Icon,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.runtime.selection_disabled() || paths.is_empty() {
            return;
        }
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        let count = paths.len();
        let first = &paths[0];
        let name = first
            .file_name()
            .map_or_else(|| first.to_string_lossy(), |name| name.to_string_lossy());
        let name = if count == 1 {
            name.into_owned()
        } else {
            format!("{name} + {}", count - 1)
        };
        let operation_id = match session.controller.create(paths) {
            Ok(id) => id,
            Err(error) => {
                session.error = Some(error);
                cx.notify();
                return;
            }
        };
        session.error = None;
        let share = Share {
            id: 0,
            source: ShareSource::Native { operation_id },
            name,
            link: None,
            contents: ShareContents::Selected(count),
            size: None,
            age: ShareAge::CreatedAt(std::time::SystemTime::now()),
            tone: Tone::Mint,
            symbol,
            state: ShareState::Preparing,
            error: None,
            authorization: None,
            relay_status: None,
        };
        self.present_native_share(share, window, cx);
    }

    fn present_native_share(
        &mut self,
        mut share: Share,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> usize {
        let id = self.next_operation();
        share.id = id;
        self.shares.insert(0, share);
        self.share_flow = ShareFlow::Running(ShareRun {
            operation: id,
            selection_id: id,
            started_at: Instant::now(),
            readiness: ShareReadiness::Pending,
        });
        self.link_disclosure = LinkDisclosure::Summary;
        self.navigate(Page::Share, window, cx);
        cx.notify();
        id
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
        let mut share = self.shares.iter().find(|share| share.id == id)?.clone();
        let ShareSource::Native { operation_id } = &share.source else {
            return None;
        };
        match session.controller.repeat(operation_id) {
            Ok(operation_id) => share.source = ShareSource::Native { operation_id },
            Err(error) => {
                session.error = Some(error);
                cx.notify();
                return None;
            }
        }
        session.error = None;
        share.link = None;
        share.error = None;
        share.relay_status = None;
        share.state = if share.authorization.is_some() {
            ShareState::AwaitingAuthorization
        } else {
            ShareState::Preparing
        };
        share.age = ShareAge::CreatedAt(std::time::SystemTime::now());
        Some(self.present_native_share(share, window, cx))
    }
}
