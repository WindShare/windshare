use super::{Coin, Payment};
use gpui::{Div, Image, ImageFormat, div, img, prelude::*, px, rgb};
use std::sync::{Arc, LazyLock};
use windshare_ui::QrCodeImage;

pub(super) const QR_SIDE: f32 = 232.;

static PROVIDERS: LazyLock<[Arc<Image>; 3]> = LazyLock::new(|| {
    [
        include_bytes!("../../../assets/payments/alipay.png").as_slice(),
        include_bytes!("../../../assets/payments/okx.png").as_slice(),
        include_bytes!("../../../assets/payments/binance.png").as_slice(),
    ]
    .map(|bytes| Arc::new(Image::from_bytes(ImageFormat::Png, bytes.to_vec())))
});

static WALLETS: LazyLock<[(Coin, QrCodeImage); Coin::ALL.len()]> = LazyLock::new(|| {
    // Bind codes to coins rather than picker positions; copy and scan share one address.
    Coin::ALL.map(|coin| {
        let code = QrCodeImage::encode(coin.wallet().address)
            .expect("bundled wallet address must fit a QR code");
        (coin, code)
    })
});

pub(super) fn code(payment: Payment) -> Div {
    match payment {
        Payment::Alipay => provider(PROVIDERS[0].clone()),
        Payment::OkxPay => provider(PROVIDERS[1].clone()),
        Payment::BinancePay => provider(PROVIDERS[2].clone()),
        Payment::Crypto(coin) => WALLETS
            .iter()
            .find(|(currency, _)| *currency == coin)
            .expect("every supported coin has a QR code")
            .1
            .view(QR_SIDE),
    }
}

fn provider(image: Arc<Image>) -> Div {
    div()
        .size(px(QR_SIDE))
        .flex_shrink_0()
        .bg(rgb(0xff_ffff))
        .child(img(image).size_full())
}
