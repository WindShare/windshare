use crate::{
    desktop::Desktop,
    live::Runtime,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use std::path::Path;
use windshare_host::{Connection, Model, SenderTrace, SenderTraceChange};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub(crate) fn diagnostics_card(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let locale = language(cx);
        let logs_available = self.log_directory().is_some();
        Some(
            theme
                .card()
                .p(px(28.))
                .gap(px(16.))
                .child(
                    row()
                        .gap(px(14.))
                        .child(theme.icon_tile(Icon::Activity, Tone::Lavender, 44.))
                        .child(theme.heading(locale.text(Text::Diagnostics), 22.)),
                )
                .child(Self::sender_trace_setting(theme, &session.model, cx))
                .child(theme.separator())
                .child(
                    row()
                        .gap(px(18.))
                        .child(
                            column()
                                .flex_1()
                                .min_w_0()
                                .gap(px(5.))
                                .child(theme.heading(locale.text(Text::LocalLogs), 16.))
                                .when(!logs_available, |el| {
                                    el.child(theme.caption(locale.text(Text::LogFolderUnavailable)))
                                }),
                        )
                        .child(
                            Self::button(
                                "open-log-folder",
                                locale.text(Text::OpenLogFolder),
                                Icon::Folder,
                                ButtonStyle::Secondary,
                                Action::OpenLogs,
                                cx,
                            )
                            .compact()
                            .disabled(!logs_available),
                        ),
                ),
        )
    }

    fn sender_trace_setting(theme: Theme, model: &Model, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let enabled = model.sender_trace == SenderTrace::On;
        let (detail, can_change) = match (
            &model.connection,
            model.sender_trace,
            model.sender_trace_change,
        ) {
            (Connection::Starting, _, _) => (Text::StartingHost, false),
            (Connection::Reconnecting(_), _, _) => (Text::ControlsReconnecting, false),
            (Connection::Connected, SenderTrace::Unavailable, _) => {
                (Text::SenderTraceUnavailable, false)
            }
            (Connection::Connected, _, SenderTraceChange::Applying) => {
                (Text::SenderTraceUpdating, false)
            }
            (Connection::Connected, _, _) => (Text::SenderTraceDetail, true),
            _ => (Text::HostClosed, false),
        };
        row()
            .gap(px(18.))
            .child(
                column()
                    .flex_1()
                    .min_w_0()
                    .gap(px(5.))
                    .child(theme.heading(locale.text(Text::SenderTrace), 16.))
                    .child(theme.caption(locale.text(detail)))
                    .when(
                        can_change && model.sender_trace_change == SenderTraceChange::Failed,
                        |el| {
                            el.child(
                                theme
                                    .caption(locale.text(Text::SenderTraceFailed))
                                    .text_color(Tone::Rose.text(theme)),
                            )
                        },
                    ),
            )
            .child(Self::setting_switch(
                theme,
                "sender-trace",
                locale.text(Text::SenderTrace),
                enabled,
                can_change.then_some(Action::SetSenderTrace(!enabled)),
                cx,
            ))
    }

    pub(crate) fn set_sender_trace(&self, enabled: bool) {
        if let Runtime::Native(session) = &self.runtime {
            session.controller.set_sender_trace(enabled);
        }
    }

    fn log_directory(&self) -> Option<&Path> {
        if let Runtime::Native(session) = &self.runtime
            && let Some(directory) = session.model.log_directory.as_deref()
        {
            return Some(directory);
        }
        crate::logging::directory()
    }

    pub(crate) fn open_logs(&mut self, cx: &mut Context<Self>) {
        let Some(path) = self.log_directory().filter(|path| path.is_dir()) else {
            tracing::warn!(
                event = "desktop.log_folder_unavailable",
                session = std::process::id()
            );
            self.notify(Text::LogFolderUnavailable, cx);
            return;
        };
        tracing::info!(
            event = "desktop.log_folder_opened",
            session = std::process::id()
        );
        cx.open_with_system(path);
    }
}
