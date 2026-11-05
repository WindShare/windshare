use super::{Coin, Payment};
use gpui::{Div, div, point, prelude::*, px, size};
use std::sync::LazyLock;
use windshare_ui::{Icon, Theme, VectorArtwork, icon};

#[derive(Clone, Copy)]
enum Mark {
    Alipay,
    Okx,
    Binance,
    Bitcoin,
    Ethereum,
    Tether,
    Solana,
    Sui,
    BitcoinCash,
}

static MARKS: LazyLock<[VectorArtwork; 9]> = LazyLock::new(|| {
    [
        include_bytes!("../../../assets/payments/icons/alipay.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/okx.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/binance.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/bitcoin.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/ethereum.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/tether.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/solana.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/sui.svg").as_slice(),
        include_bytes!("../../../assets/payments/icons/bitcoincash.svg").as_slice(),
    ]
    .map(|source| VectorArtwork::from_svg(source).expect("bundled payment mark must be valid"))
});

fn mark(mark: Mark, side: f32) -> Div {
    div()
        .relative()
        .size(px(side))
        .flex_shrink_0()
        .child(MARKS[mark as usize].sprite(
            size(px(side), px(side)),
            point(px(side / 2.), px(side / 2.)),
        ))
}

pub(super) fn coin(coin: Coin, side: f32) -> Div {
    mark(
        match coin {
            Coin::Btc => Mark::Bitcoin,
            Coin::Eth => Mark::Ethereum,
            Coin::UsdtTrc20 => Mark::Tether,
            Coin::Sol => Mark::Solana,
            Coin::Sui => Mark::Sui,
            Coin::Bch => Mark::BitcoinCash,
            Coin::Bnb => Mark::Binance,
        },
        side,
    )
}

pub(super) fn payment(payment: Payment, theme: Theme, side: f32) -> Div {
    match payment {
        Payment::Alipay => mark(Mark::Alipay, side),
        Payment::OkxPay => mark(Mark::Okx, side),
        Payment::BinancePay => mark(Mark::Binance, side),
        Payment::Crypto(_) => div()
            .size(px(side))
            .flex_shrink_0()
            .child(icon(Icon::Grid, side).text_color(theme.accent_text())),
    }
}
