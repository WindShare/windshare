fn main() {
    println!("cargo:rerun-if-changed=exports.def");
    if std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc") {
        let manifest = std::env::var("CARGO_MANIFEST_DIR").expect("Cargo manifest directory");
        // COM entry points are discovered dynamically and must not enter the import library.
        println!("cargo:rustc-cdylib-link-arg=/DEF:{manifest}/exports.def");
    }
}
