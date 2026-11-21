use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use std::{
    io::Write,
    path::Path,
    time::{SystemTime, UNIX_EPOCH},
};
use windshare_relay_access::trust::{Generation, MAX_CERTIFICATE_LIFETIME};

fn secret(path: &Path) -> Result<SigningKey> {
    if path.exists() {
        let bytes = URL_SAFE_NO_PAD.decode(std::fs::read_to_string(path)?.trim())?;
        return Ok(SigningKey::from_bytes(
            &bytes
                .try_into()
                .map_err(|_| anyhow::anyhow!("seed width"))?,
        ));
    }
    let mut bytes = [0; 32];
    getrandom::fill(&mut bytes).map_err(|error| anyhow::anyhow!("{error}"))?;
    write_new(path, URL_SAFE_NO_PAD.encode(bytes).as_bytes())?;
    Ok(SigningKey::from_bytes(&bytes))
}
fn write_new(path: &Path, bytes: &[u8]) -> Result<()> {
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    Ok(())
}
fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if let [command, document, seed] = args.as_slice()
        && command == "verify-publisher"
    {
        use windshare_network_authority::fleet::publication::{Publisher, Release};
        let release = Release::bundled()?;
        let cert = serde_json::from_slice(&std::fs::read(document)?)?;
        return Publisher::new(&release, cert, std::fs::read_to_string(seed)?.trim())?
            .check_release(
                &release,
                SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs(),
            );
    }
    if let [command, document, seed] = args.as_slice()
        && command == "verify"
    {
        let trust: serde_json::Value = serde_json::from_slice(&std::fs::read(document)?)?;
        let key = windshare_network_authority::Signing::load(
            trust["root"].as_str().context("trust root")?,
            trust["certificate"].as_str().context("trust certificate")?,
            std::fs::read_to_string(seed)?.trim(),
        )?;
        ensure!(
            key.generation
                .usable(SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs()),
            "certificate outside validity"
        );
        return Ok(());
    }
    let [directory, generation] = args.as_slice() else {
        anyhow::bail!("Usage: network-authority-keys <offline-key-directory> <generation>")
    };
    let generation: u64 = generation.parse()?;
    ensure!(generation > 0, "generation must be positive");
    let directory = Path::new(directory);
    std::fs::create_dir_all(directory)?;
    let root = secret(&directory.join("root.seed"))?;
    let online_path = directory.join(format!("online-{generation}.seed"));
    ensure!(
        !online_path.exists(),
        "generation already exists; use a new generation"
    );
    let online = secret(&online_path)?;
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs();
    let certificate = Generation {
        issuer: root.verifying_key().to_bytes(),
        number: generation,
        key: online.verifying_key().to_bytes(),
        not_before: now,
        expires_at: now + MAX_CERTIFICATE_LIFETIME,
    };
    let public = serde_json::json!({"root":URL_SAFE_NO_PAD.encode(certificate.issuer),
        "certificate":URL_SAFE_NO_PAD.encode(certificate.certify(&root).map_err(|error|anyhow::anyhow!("{error:?}"))?),
        "generation":generation,"expires_at":certificate.expires_at});
    let trust_path = directory.join(format!("trust-{generation}.json"));
    write_new(&trust_path, &serde_json::to_vec_pretty(&public)?).context("write trust document")?;
    println!(
        "{}",
        serde_json::json!({"trust":trust_path,"online_seed":online_path,"expires_at":certificate.expires_at})
    );
    Ok(())
}
