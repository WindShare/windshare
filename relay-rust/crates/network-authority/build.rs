use std::{env, fs, path::PathBuf};

fn main() {
    // Include removed aliases as well as the release head. The publisher verifies
    // each signature before treating these bytes as permanent bindings.
    // Read CARGO_MANIFEST_DIR at runtime so a build script binary reused across
    // worktrees reads the active checkout.
    let directory = PathBuf::from(env::var_os("CARGO_MANIFEST_DIR").expect("manifest directory"))
        .join("../../../deploy/network/history");
    println!("cargo:rerun-if-changed={}", directory.display());
    let mut paths: Vec<_> = fs::read_dir(directory)
        .expect("directory release history")
        .map(|item| item.expect("history entry").path())
        .filter(|path| path.extension().is_some_and(|ext| ext == "json"))
        .collect();
    paths.sort();
    let files = paths
        .iter()
        .map(|p| {
            format!(
                "include_str!(\"{}\")",
                p.display().to_string().escape_default()
            )
        })
        .collect::<Vec<_>>()
        .join(",");
    let output = PathBuf::from(env::var_os("OUT_DIR").unwrap()).join("directory_history.rs");
    fs::write(output, format!("&[{files}]")).unwrap();
}
