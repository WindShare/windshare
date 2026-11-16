use crate::{Activation, EntrySource, ShareIntent};
use std::{
    ffi::OsString,
    path::{Path, PathBuf},
};

pub enum Launch {
    Application {
        activation: Activation,
        compact: bool,
    },
    Dispatch {
        endpoint: String,
    },
    Preview {
        compact: bool,
        delay_ms: u64,
    },
    HostCheck,
    IdentityCheck,
}

const DEFAULT_PREVIEW_DELAY_MS: u64 = 1_900;

/// # Errors
/// Returns an actionable argument error without converting native paths to text.
pub fn parse(arguments: impl IntoIterator<Item = OsString>, cwd: &Path) -> Result<Launch, String> {
    let mut arguments = arguments.into_iter();
    let mut compact = false;
    let mut preview = false;
    let mut delay_ms = DEFAULT_PREVIEW_DELAY_MS;
    while let Some(argument) = arguments.next() {
        match argument.to_str() {
            Some("--host-check") => return exclusive(arguments, Launch::HostCheck),
            Some("--identity-check") => return exclusive(arguments, Launch::IdentityCheck),
            Some("--dispatch") => {
                let endpoint = arguments
                    .next()
                    .ok_or("Missing dispatcher endpoint")?
                    .into_string()
                    .map_err(|_| "Invalid dispatcher endpoint")?;
                uuid::Uuid::parse_str(&endpoint).map_err(|_| "Invalid dispatcher endpoint")?;
                return exclusive(arguments, Launch::Dispatch { endpoint });
            }
            Some("--compact") => compact = true,
            Some("--preview") => preview = true,
            Some("--live-share") if !preview => {
                let roots = arguments
                    .map(|path| absolute(PathBuf::from(path), cwd))
                    .collect::<Result<Vec<_>, _>>()?;
                let intent = ShareIntent::new(EntrySource::CommandLine, roots);
                intent.validate()?;
                return Ok(Launch::Application {
                    activation: Activation::ShareSelection(intent),
                    compact,
                });
            }
            Some(value) if value.starts_with("--share-delay-ms=") => {
                delay_ms = value["--share-delay-ms=".len()..]
                    .parse()
                    .map_err(|_| "--share-delay-ms requires an unsigned integer")?;
            }
            _ => {
                return Err(format!(
                    "Unsupported launch argument: {}",
                    argument.display()
                ));
            }
        }
    }
    Ok(if preview {
        Launch::Preview { compact, delay_ms }
    } else {
        Launch::Application {
            activation: Activation::ShowWindow,
            compact,
        }
    })
}

fn exclusive(
    mut arguments: impl Iterator<Item = OsString>,
    launch: Launch,
) -> Result<Launch, String> {
    if arguments.next().is_some() {
        Err("Unexpected arguments after the launch mode".into())
    } else {
        Ok(launch)
    }
}

fn absolute(path: PathBuf, cwd: &Path) -> Result<PathBuf, String> {
    if path.as_os_str().is_empty() {
        return Err("A selected path is empty".into());
    }
    #[cfg(windows)]
    if !path.is_absolute()
        && (path.has_root()
            || path
                .components()
                .any(|part| matches!(part, std::path::Component::Prefix(_))))
    {
        // Drive-relative paths depend on per-drive process state, not just cwd.
        return std::path::absolute(path).map_err(|error| error.to_string());
    }
    Ok(if path.is_absolute() {
        path
    } else {
        cwd.join(path)
    })
}
