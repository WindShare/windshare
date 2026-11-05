mod artwork;
mod qr;
mod view;

use crate::localization::{Language, Text};

const EVM_ADDRESS: &str = "0x5eebb308017317b059b11b00aa90ad2a21feb256";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Coin {
    Btc,
    Eth,
    #[default]
    UsdtTrc20,
    Sol,
    Sui,
    Bch,
    Bnb,
}

pub struct Wallet {
    pub name: &'static str,
    pub network: &'static str,
    pub address: &'static str,
}

impl Coin {
    pub const ALL: [Self; 7] = [
        Self::Btc,
        Self::Eth,
        Self::UsdtTrc20,
        Self::Sol,
        Self::Sui,
        Self::Bch,
        Self::Bnb,
    ];

    pub const fn ticker(self) -> &'static str {
        match self {
            Self::Btc => "BTC",
            Self::Eth => "ETH",
            Self::UsdtTrc20 => "USDT",
            Self::Sol => "SOL",
            Self::Sui => "SUI",
            Self::Bch => "BCH",
            Self::Bnb => "BNB",
        }
    }

    pub const fn wallet(self) -> Wallet {
        let (name, network, address) = match self {
            Self::Btc => (
                "Bitcoin",
                "Bitcoin",
                "bc1p8f5ar05p08e3ut8k6r25v0tu5tewq4qw7mnx87w34m9fzpkqpc7saq3aeh",
            ),
            Self::Eth => ("Ethereum", "Ethereum", EVM_ADDRESS),
            Self::UsdtTrc20 => (
                "Tether",
                "TRON · TRC20",
                "THJzAL1ZQuVrn5cGPaecCnp7PfTr9Eh4mm",
            ),
            Self::Sol => (
                "Solana",
                "Solana",
                "A5ZvjhS8rbajgEoZ5MPMeFKbMVBjTngJE2KDqSFd8YZU",
            ),
            Self::Sui => (
                "Sui",
                "Sui",
                "0x21c41c0a31ea5a30a3e7644d045c7f565fb8478efded149e0da0bc1516410539",
            ),
            Self::Bch => (
                "Bitcoin Cash",
                "Bitcoin Cash",
                "qzxgwva627638dwk42wcc99ef89am2jnzvr3nfa9m2",
            ),
            Self::Bnb => ("BNB", "BNB Smart Chain · BEP20", EVM_ADDRESS),
        };
        Wallet {
            name,
            network,
            address,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Payment {
    Alipay,
    OkxPay,
    BinancePay,
    Crypto(Coin),
}

impl Payment {
    pub const fn label(self, language: Language) -> &'static str {
        match self {
            Self::Alipay => language.text(Text::SuperAlipay),
            Self::OkxPay => "OKX Pay",
            Self::BinancePay => "Binance Pay",
            Self::Crypto(_) => language.text(Text::SuperCrypto),
        }
    }
}
