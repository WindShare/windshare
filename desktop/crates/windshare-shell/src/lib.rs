#[cfg(windows)]
mod command;
#[cfg(windows)]
mod factory;

pub fn command_clsid() -> u128 {
    if option_env!("WINDSHARE_INSTALL_CHANNEL") == Some("release") {
        0xc4c39339_1ade_4a87_ae3f_20db91eac9a1
    } else {
        0xe09c97a5_7670_4317_877f_d14dbf33c9ab
    }
}
