mod account;
mod account_view;
mod application;
pub use account::{AccountAction, AccountForm};
mod blocking;
mod presentation;
mod projection;
mod quit;
mod selection;
mod session;
mod setup;
mod setup_feedback;
pub use setup::SetupForm;

use crate::desktop::Desktop;
use gpui::{AppContext, Bounds, TitlebarOptions, WindowBounds, WindowOptions, px, size};
pub use session::Runtime;
use windshare_activation::Activation;
use windshare_host::Controller;

pub fn run(activation: Activation, compact: bool) -> Result<(), String> {
    #[cfg(windows)]
    let scope =
        windshare_activation::windows::Scope::current().map_err(|error| error.to_string())?;
    #[cfg(windows)]
    let Some(_owner) = scope.elect().map_err(|error| error.to_string())? else {
        return windshare_activation::windows::dispatch::deliver(
            &scope,
            &activation,
            &std::env::current_exe().map_err(|error| error.to_string())?,
        )
        .map_err(|error| error.to_string());
    };
    let application = application::Application::new();
    #[cfg(windows)]
    let _server = {
        let receiver = application.clone();
        windshare_activation::windows::Server::start(scope, move |request| {
            receiver.receive(request)
        })
        .map_err(|error| error.to_string())?
    };
    let controller = Controller::start().map_err(|error| error.to_string())?;
    application.start(controller.clone());
    application.submit(activation)?;
    let application_controller = controller.clone();
    let event_application = application.clone();
    gpui_platform::application()
        .with_assets(crate::DesktopAssets)
        .with_quit_mode(gpui::QuitMode::LastWindowClosed)
        .run(move |cx| {
            windshare_ui::init(cx);
            crate::preferences::init(cx);
            crate::artwork::Artwork::preload();
            let quit_application = event_application.clone();
            cx.on_app_quit(move |_| {
                // OS quit paths also seal admission. Joining belongs after the event
                // loop so network cleanup cannot exhaust GPUI's short quit budget.
                quit_application.begin_shutdown();
                std::future::ready(())
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
                    let mut desktop = Desktop::new(
                        window,
                        Runtime::native(application_controller, event_application),
                        cx,
                    );
                    desktop.consume_activations(window, cx);
                    desktop
                })
            })
            .expect("open WindShare window");
            cx.activate(true);
        });
    tracing::info!(
        event = "desktop.event_loop_stopped",
        session = std::process::id()
    );
    // Keep process ownership until cleanup settles, without keeping a window open.
    application.begin_shutdown();
    controller.join()
}
