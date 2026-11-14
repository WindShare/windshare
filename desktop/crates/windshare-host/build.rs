use std::{env, path::PathBuf, process::Command};

fn main() {
    let manifest = PathBuf::from(env::var_os("CARGO_MANIFEST_DIR").expect("manifest directory"));
    let root = manifest.ancestors().nth(3).expect("repository root");
    let target = env::var("TARGET").expect("target");
    assert_eq!(
        env::var("HOST").expect("host"),
        target,
        "Build the Go host on the native desktop target"
    );
    let name = match env::var("CARGO_CFG_TARGET_OS").as_deref() {
        Ok("windows") => "windshare_host.dll",
        Ok("macos") => "libwindshare_host.dylib",
        Ok("linux") => "libwindshare_host.so",
        other => panic!("Unsupported desktop host: {other:?}"),
    };
    println!("cargo:rerun-if-env-changed=CC");
    println!("cargo:rerun-if-env-changed=GOFLAGS");
    for path in [
        "go.mod",
        "go.sum",
        "adapters",
        "connectivity",
        "core",
        "engine",
        "deploy/network",
        "host",
        "osfs",
        "relay/protocol",
        "transport",
        "internal",
        "cmd/windshare-host",
        "third_party/pion",
    ] {
        println!("cargo:rerun-if-changed={}", root.join(path).display());
    }
    let output = PathBuf::from(env::var_os("OUT_DIR").expect("build output"));
    let library = output.join(name);
    let status = Command::new("go")
        .current_dir(root)
        .env("GOWORK", "off")
        .env("GOTOOLCHAIN", "local")
        .env("CGO_ENABLED", "1")
        .args(["build", "-trimpath", "-buildmode=c-shared", "-o"])
        .arg(&library)
        .arg("./cmd/windshare-host")
        .status()
        .expect("Install Go and a C compiler (MinGW-w64 GCC on Windows)");
    assert!(status.success(), "Go host build failed");
}
