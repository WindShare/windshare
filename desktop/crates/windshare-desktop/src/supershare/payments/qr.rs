use super::{Coin, Payment};
use gpui::{
    Bounds, Div, Image, ImageFormat, canvas, div, fill, img, point, prelude::*, px, rgb, size,
};
use qrcode::{Color, EcLevel, QrCode};
use std::sync::{Arc, LazyLock};

pub(super) const QR_SIDE: f32 = 232.;
const QUIET_MODULES: u16 = 4;

static PROVIDERS: LazyLock<[Arc<Image>; 3]> = LazyLock::new(|| {
    [
        include_bytes!("../../../assets/payments/alipay.png").as_slice(),
        include_bytes!("../../../assets/payments/okx.png").as_slice(),
        include_bytes!("../../../assets/payments/binance.png").as_slice(),
    ]
    .map(|bytes| Arc::new(Image::from_bytes(ImageFormat::Png, bytes.to_vec())))
});

static WALLETS: LazyLock<[(Coin, QrCode); Coin::ALL.len()]> = LazyLock::new(|| {
    // Bind codes to coins rather than picker positions; copy and scan share one address.
    Coin::ALL.map(|coin| {
        let code = QrCode::with_error_correction_level(coin.wallet().address, EcLevel::M)
            .expect("bundled wallet address must fit a QR code");
        (coin, code)
    })
});

pub(super) fn code(payment: Payment) -> Div {
    let frame = div().size(px(QR_SIDE)).flex_shrink_0().bg(rgb(0xff_ffff));
    match payment {
        Payment::Alipay => frame.child(img(PROVIDERS[0].clone()).size_full()),
        Payment::OkxPay => frame.child(img(PROVIDERS[1].clone()).size_full()),
        Payment::BinancePay => frame.child(img(PROVIDERS[2].clone()).size_full()),
        Payment::Crypto(coin) => frame.child(wallet(coin)),
    }
}

fn wallet(coin: Coin) -> impl IntoElement {
    let (_, code) = WALLETS
        .iter()
        .find(|(currency, _)| *currency == coin)
        .expect("every supported coin has a QR code");
    let width = u16::try_from(code.width()).expect("QR dimensions fit u16");
    canvas(
        |_, _, _| (),
        move |bounds, (), window, _| {
            // Snap whole modules to physical pixels; antialiased edges make phone scanning harder.
            let scale = window.scale_factor();
            let modules = f32::from(width + QUIET_MODULES * 2);
            let cell = (f32::from(bounds.size.width) * scale / modules).floor() / scale;
            let drawn_side = cell * f32::from(width);
            let origin = point(
                px(
                    ((f32::from(bounds.left()) + (QR_SIDE - drawn_side) / 2.) * scale).round()
                        / scale,
                ),
                px(
                    ((f32::from(bounds.top()) + (QR_SIDE - drawn_side) / 2.) * scale).round()
                        / scale,
                ),
            );
            for y in 0..width {
                for x in 0..width {
                    if code[(usize::from(x), usize::from(y))] == Color::Dark {
                        window.paint_quad(fill(
                            Bounds::new(
                                origin + point(px(f32::from(x) * cell), px(f32::from(y) * cell)),
                                size(px(cell), px(cell)),
                            ),
                            rgb(0x00_0000),
                        ));
                    }
                }
            }
        },
    )
    .size_full()
}
