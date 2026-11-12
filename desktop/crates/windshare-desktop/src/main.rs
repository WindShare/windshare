#![cfg_attr(all(target_os = "windows", not(test)), windows_subsystem = "windows")]

mod artwork;
mod commands;
mod desktop;
mod flow_state;
mod layout;
mod live;
mod localization;
mod logging;
mod model;
mod motion;
mod motion_probe;
mod overlays;
mod pages;
mod preferences;
#[cfg(feature = "motion-profile")]
mod profiling;
mod receiving;
mod share_animation;
mod share_timing;
mod share_views;
mod sharing;
mod shell;
mod supershare;
mod transfer_views;

use desktop::Desktop;
use gpui::{
    AppContext, AssetSource, Bounds, SharedString, TitlebarOptions, WindowBounds, WindowOptions,
    px, size,
};
use std::{borrow::Cow, time::Duration};

const DEFAULT_PREVIEW_SHARE_MS: u64 = 1900;

struct DesktopAssets;
impl AssetSource for DesktopAssets {
    fn load(&self, path: &str) -> gpui::Result<Option<Cow<'static, [u8]>>> {
        if path == "brand/breeze.svg" {
            return Ok(Some(Cow::Borrowed(include_bytes!("../assets/breeze.svg"))));
        }
        windshare_ui::Assets.load(path)
    }
    fn list(&self, _: &str) -> gpui::Result<Vec<SharedString>> {
        Ok(vec![])
    }
}

fn main() -> std::process::ExitCode {
    let _logs = logging::start();
    #[cfg(feature = "motion-profile")]
    let _capture = profiling::start();
    let arguments: Vec<String> = std::env::args().skip(1).collect();
    if arguments.iter().any(|arg| arg == "--host-check") {
        if let Err(error) = windshare_host::check_host() {
            tracing::error!(event = "desktop.host_check_failed", %error);
            return std::process::ExitCode::FAILURE;
        }
        return std::process::ExitCode::SUCCESS;
    }
    if !arguments.iter().any(|arg| arg == "--preview") {
        let paths = arguments
            .iter()
            .position(|arg| arg == "--live-share")
            .map(|index| {
                arguments[index + 1..]
                    .iter()
                    .map(std::path::PathBuf::from)
                    .collect()
            })
            .unwrap_or_default();
        let compact = arguments.iter().any(|arg| arg == "--compact");
        if let Err(error) = live::run(paths, compact) {
            tracing::error!(event = "desktop.live_share_failed", %error);
            return std::process::ExitCode::FAILURE;
        }
        return std::process::ExitCode::SUCCESS;
    }
    let compact = std::env::args().any(|arg| arg == "--compact");
    let share_delay_ms = std::env::args()
        .find_map(|arg| arg.strip_prefix("--share-delay-ms=").map(str::to_owned))
        .map(|value| {
            value
                .parse::<u32>()
                .expect("--share-delay-ms requires an unsigned integer")
        })
        .map_or(DEFAULT_PREVIEW_SHARE_MS, u64::from);
    let preview_share_delay = Duration::from_millis(share_delay_ms);
    gpui_platform::application()
        .with_assets(DesktopAssets)
        .run(move |cx| {
            windshare_ui::init(cx);
            preferences::init(cx);
            artwork::Artwork::preload();
            let dimensions = if compact {
                size(px(960.), px(760.))
            } else {
                size(px(1360.), px(900.))
            };
            let options = WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(Bounds::centered(
                    None, dimensions, cx,
                ))),
                titlebar: Some(TitlebarOptions {
                    title: Some(
                        localization::language(cx)
                            .text(localization::Text::WindowTitle)
                            .into(),
                    ),
                    appears_transparent: true,
                    ..Default::default()
                }),
                window_min_size: Some(size(px(820.), px(620.))),
                app_id: Some("windshare.desktop.preview".into()),
                ..Default::default()
            };
            cx.open_window(options, |window, cx| {
                cx.new(|cx| {
                    Desktop::new(
                        window,
                        live::Runtime::Preview {
                            delay: preview_share_delay,
                        },
                        cx,
                    )
                })
            })
            .expect("WindShare needs an available native GPU window");
            tracing::info!(
                operation = "desktop.started",
                session = std::process::id(),
                share_delay_ms,
                preview = true
            );
            cx.on_window_closed(|cx, _| {
                if cx.windows().is_empty() {
                    cx.quit();
                }
            })
            .detach();
            cx.activate(true);
        });
    std::process::ExitCode::SUCCESS
}
