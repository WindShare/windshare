"""Publish captures without workstation identities; hashes describe published bytes."""
import hashlib
import ipaddress
import json
from pathlib import Path, PurePosixPath
import re
import zipfile

PATH = re.compile(
    r"""(?<![\w])(?:[A-Za-z]:[\\/]+|/(?:home|Users)/[^/\s"']+/|/root/)"""
    r"""(?:[^\s"'<>|()\[\]{},]+| (?=[^\\/\r\n"'<>|():\[\]{},]*[\\/]))+"""
)
IPV4 = re.compile(r"(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])")
HASH = re.compile(r"\b[0-9a-f]{64}\b")
IDENTITY = re.compile(
    r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}|"
    r"BEGIN [\w ]*PRIVATE KEY|(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}|"
    r"(?:Bearer|Basic)\s+[A-Za-z0-9+/_.=-]{16,}|"
    r"""(?i:password|passwd|api[_-]?key|access[_-]?token|secret)\s*[:=]\s*["']?[^\s,"'}]{8,}"""
)
DOCUMENTATION_NET = ipaddress.ip_network("192.0.2.0/24")
PUBLICATION = "publication.json"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def private_address(value):
    address = ipaddress.ip_address(value)
    return not (address.is_loopback or address.is_unspecified or address in DOCUMENTATION_NET)


def portable_path(match):
    parts = re.split(r"[\\/]+", match.group())
    for marker in (".build", "results"):
        if marker in parts:
            return "/".join(parts[parts.index(marker):])
    # Retain source filenames/line numbers for diagnostics, never their parent directories.
    leaf = parts[-1]
    return "local/" + leaf if "." in leaf else "<local-path>"


def public_text(text, addresses):
    if IDENTITY.search(text):
        raise ValueError("Potential credential or personal email in publication input")
    text = PATH.sub(portable_path, text)
    return IPV4.sub(lambda match: addresses.get(match.group(), match.group()), text)


def public_value(value, addresses):
    if isinstance(value, str):
        return public_text(value, addresses)
    if isinstance(value, list):
        return [public_value(item, addresses) for item in value]
    if isinstance(value, dict):
        return {public_text(key, addresses): public_value(item, addresses) for key, item in value.items()}
    return value


def public_bytes(name, data, addresses):
    # Repository checkouts use LF; published digests must survive git normalization.
    text = data.decode("utf-8-sig").replace("\r\n", "\n")
    data = text.encode()
    # Failed exploratory runs may intentionally retain an empty summary.
    if not text.strip():
        return data
    suffix = PurePosixPath(name).suffix
    if suffix == ".json":
        value = json.loads(text)
        public = public_value(value, addresses)
        return (json.dumps(public, indent=2) + "\n").encode() if public != value else data
    if suffix == ".jsonl":
        lines = []
        for line in text.splitlines(keepends=True):
            if not line.strip():
                lines.append(line)
                continue
            value = json.loads(line)
            public = public_value(value, addresses)
            lines.append(json.dumps(public, separators=(",", ":")) + "\n" if public != value else line)
        return "".join(lines).encode()
    return public_text(text, addresses).encode()


def hash_updates(archive_path):
    with zipfile.ZipFile(archive_path) as archive:
        publication = json.loads(archive.read(PUBLICATION))
    updates = dict(publication["reference_hash_updates"])
    updates.update({entry["original_sha256"]: entry["published_sha256"]
                    for entry in publication["changed_files"].values()})
    return updates


def write_archive(destination, members, reference_hash_updates=None):
    """Keep raw captures local; normalize archive contents and repair their hash graph."""
    texts = {}
    for name, data in members.items():
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or "\\" in name or ":" in name or name == PUBLICATION:
            raise ValueError("Unsafe or reserved archive member")
        texts[name] = data.decode("utf-8-sig")
    addresses = sorted({match.group() for text in texts.values()
                        for match in IPV4.finditer(text) if private_address(match.group())})
    if len(addresses) > DOCUMENTATION_NET.num_addresses - 2:
        raise ValueError("Too many endpoints for documentation address mapping")
    replacements = {value: str(DOCUMENTATION_NET[index + 1]) for index, value in enumerate(addresses)}
    normalized = {name: public_bytes(name, data, replacements) for name, data in members.items()}
    published = normalized
    references = dict(reference_hash_updates or {})
    # Trace -> run ledger -> summary references must settle before their hashes are frozen.
    for _ in range(len(members) + 1):
        updates = references | {digest(members[name]): digest(data) for name, data in published.items()
                                if members[name] != data}
        rewritten = {name: HASH.sub(lambda match: updates.get(match.group(), match.group()),
                                    data.decode()).encode() for name, data in normalized.items()}
        if rewritten == published:
            break
        published = rewritten
    else:
        raise ValueError("Evidence hashes contain a cycle")
    publication = {
        "format": "privacy-normalized-v1",
        "normalization": "Local paths and network endpoints are replaced; measured values are unchanged.",
        "hash_scope": "References describe published bytes; original digests retain capture provenance.",
        "reference_hash_updates": references,
        "changed_files": {name: {"original_sha256": digest(members[name]), "published_sha256": digest(data)}
                          for name, data in sorted(published.items()) if data != members[name]},
    }
    published[PUBLICATION] = (json.dumps(publication, indent=2) + "\n").encode()
    destination = Path(destination)
    pending = destination.with_suffix(destination.suffix + ".tmp")
    # Fresh ZIP entries omit original filesystem timestamps, owners and extra metadata.
    with zipfile.ZipFile(pending, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, data in sorted(published.items()):
            entry = zipfile.ZipInfo(name)
            entry.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(entry, data, compresslevel=9)
    pending.replace(destination)
    return published
