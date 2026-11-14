mod blocking;
mod projection;
mod selection;
mod session;
mod setup;
pub use setup::SetupForm;

use crate::desktop::Desktop;
use gpui::{AppContext, Bounds, TitlebarOptions, WindowBounds, WindowOptions, px, size};
pub use session::Runtime;
use std::path::PathBuf;
use windshare_host::Controller;

pub fn run(paths: Vec<PathBuf>, compact: bool) -> Result<(), String> {
    let controller = Controller::start().map_err(|error| error.to_string())?;
    let application_controller = controller.clone();
    gpui_platform::application()
        .with_assets(crate::DesktopAssets)
        .run(move |cx| {
            windshare_ui::init(cx);
            crate::preferences::init(cx);
            crate::artwork::Artwork::preload();
            let quit_controller = application_controller.clone();
            cx.on_app_quit(move |cx| {
                quit_controller.begin_shutdown();
                let controller = quit_controller.clone();
                cx.background_executor().spawn(async move {
                    let _ = controller.join();
                })
            })
            .detach();
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
                    title: Some("WindShare".into()),
                    appears_transparent: true,
                    ..Default::default()
                }),
                window_min_size: Some(size(px(820.), px(620.))),
                app_id: Some("windshare.desktop".into()),
                ..Default::default()
            };
            cx.open_window(options, |window, cx| {
                cx.new(|cx| {
                    let mut desktop =
                        Desktop::new(window, Runtime::native(application_controller), cx);
                    desktop.create_native_share(paths, windshare_ui::Icon::Folder, window, cx);
                    desktop
                })
            })
            .expect("open WindShare window");
            cx.activate(true);
        });
    // GPUI quit handlers have a timeout. Process exit still waits for the host,
    // including OS quit paths that have already ended the event loop.
    controller.join()
}
