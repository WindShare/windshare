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
use gpui::{AppContext, Bounds, TitlebarOptions, WindowBounds, WindowOptions, px, size};
use std::time::Duration;
use windshare_ui::Assets as DesktopAssets;

fn main() -> std::process::ExitCode {
    let _logs = logging::start();
    #[cfg(feature = "motion-profile")]
    let _capture = profiling::start();
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(event = "desktop.launch_failed", %error);
            #[cfg(windows)]
            if !std::env::args_os()
                .nth(1)
                .is_some_and(|arg| arg == "--host-check" || arg == "--identity-check")
            {
                windshare_activation::windows::show_failure(&error);
            }
            std::process::ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), String> {
    use windshare_activation::launch::{self, Launch};
    let arguments: Vec<_> = std::env::args_os().skip(1).collect();
    let diagnostic = arguments.len() == 1
        && (arguments[0] == "--host-check" || arguments[0] == "--identity-check");
    #[cfg(windows)]
    if !diagnostic
        && windshare_activation::windows::share_target::handle_activation()
            .map_err(|error| error.to_string())?
    {
        return Ok(());
    }
    let cwd = std::env::current_dir().map_err(|error| error.to_string())?;
    #[cfg(not(windows))]
    let _ = diagnostic;
    match launch::parse(arguments, &cwd)? {
        Launch::HostCheck => windshare_host::check_host().map_err(|error| error.to_string()),
        Launch::IdentityCheck => {
            #[cfg(windows)]
            return windshare_activation::windows::identity_check()
                .map_err(|error| error.to_string());
            #[cfg(not(windows))]
            Err("Package identity is only supported on Windows".into())
        }
        Launch::Dispatch { endpoint } => {
            #[cfg(windows)]
            return windshare_activation::windows::dispatch::receive(&endpoint)
                .map_err(|error| error.to_string());
            #[cfg(not(windows))]
            {
                let _ = endpoint;
                Err("Explorer dispatch is only supported on Windows".into())
            }
        }
        Launch::Application {
            activation,
            compact,
        } => live::run(activation, compact),
        Launch::Preview { compact, delay_ms } => {
            run_preview(compact, delay_ms);
            Ok(())
        }
    }
}

fn run_preview(compact: bool, share_delay_ms: u64) {
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
}
