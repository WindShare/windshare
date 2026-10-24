"""Publish performance evidence without exporting machine or connection identities.

Raw reports stay in ignored .tmp directories. This projection preserves numerical
measurements and content hashes while removing fields that can identify a lab or
carry credentials. Diagnostic logs are not an authority to publish private data.
"""
import ipaddress
import json
from pathlib import Path
import re

_PRIVATE_FIELDS = {
    "password", "secret", "sharedsecret", "privatekey", "keyfilename", "key",
    "credential", "credentials", "authorization", "accesstoken", "refreshtoken",
    "apikey", "sharekey", "readcapability", "username", "usernamefragment",
    "address", "localaddress", "remoteaddress", "hostname", "host", "publicip",
    "ip", "port", "localport", "remoteport",
    "fingerprint", "foundation", "certificate", "sdp",
}
_PRIVATE_LOGS = {
    "stdout", "stderr", "serverevents", "servererrors", "firewall",
    "initialgitstatus", "stagedpathsatverification",
}
_ID_FIELDS = {
    "id", "localcandidateid", "remotecandidateid", "transportid",
    "selectedcandidatepairid", "protocolsessionid", "peerpathid", "peerattemptid",
    "networkgenerationid", "iceprofileid", "fileid", "shareid", "keyid",
    "pathid", "trialpath", "runtimerunid", "receiveoperationid", "transferjobid",
    "operationid", "outputsessionid",
}
_WINDOWS_PATH = re.compile(r"(?i)(?:file:///)?[a-z]:[\\/][^\r\n\t\"'<>|]*")
_HOME_PATH = re.compile(r"/(?:home|Users)/[^\r\n\t\"'<>| ]+")
_URL = re.compile(r"(?i)\b(?:https?|wss?|turns?|stuns?):(?:/{2})?[^\s\"'<>]+")
_IPV4 = re.compile(r"(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])")
_IPV6 = re.compile(r"(?<!\w)(?:[0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F:.%]+")
_UUID = re.compile(r"(?i)\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b")
_EMAIL = re.compile(r"(?i)\b[\w.+-]+@[\w.-]+\.[a-z]{2,}\b")
_SSH_TARGET = re.compile(r"\b[\w.-]+@(?:<ip-address>|[\w.-]+)")
_MDNS = re.compile(r"(?i)\b[\w-]+\.local\b")
_PEM = re.compile(r"-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----", re.S)
_SECRET_ASSIGNMENT = re.compile(r"(?i)((?:password|secret|token|api[_-]?key|--key)\s*(?:[=:]|\s)\s*)[^\s,;\"]+")

def _field(name):
    return re.sub(r"[^a-z0-9]", "", name.lower())

class PublicEvidence:
    def __init__(self):
        self.identifiers = {}
        self.private_terms = set()
        self.next_identifier = 1

    def text(self, value):
        for term in sorted(self.private_terms, key=len, reverse=True):
            value = value.replace(term, '<private-value>')
        value = _PEM.sub("<private-key>", value)
        value = _WINDOWS_PATH.sub("<local-path>", value)
        value = _HOME_PATH.sub("<local-path>", value)
        value = _URL.sub("<endpoint-url>", value)
        def address(match):
            try:
                ipaddress.ip_address(match.group())
                return "<ip-address>"
            except ValueError:
                # Four-component browser versions are not network addresses.
                return match.group()
        value = _IPV4.sub(address, value)
        value = _IPV6.sub(address, value)
        value = _EMAIL.sub("<email-address>", value)
        value = _SSH_TARGET.sub("<ssh-target>", value)
        value = _UUID.sub("<opaque-id>", value)
        value = _MDNS.sub("<mdns-host>", value)
        return _SECRET_ASSIGNMENT.sub(r"\1<redacted>", value)

    def project(self, value):
        def collect(item, key=""):
            if isinstance(item, dict):
                for name, child in item.items():
                    collect(child, name)
            elif isinstance(item, list):
                for child in item:
                    collect(child, key)
            elif isinstance(item, str):
                # Re-exporting public evidence must preserve existing aliases and
                # keep newly covered identifiers out of their namespace.
                if re.fullmatch(r"identifier-\d+", item):
                    self.identifiers[item] = item
                    self.next_identifier = max(self.next_identifier, int(item.split("-")[1]) + 1)
                if _field(key) in {"endpoint", "hostname", "username", "password", "secret", "sharedsecret"}:
                    if len(item) > 4 and not item.startswith("<") and item not in {"public", "cloudflare", "isolated-pion"}:
                        self.private_terms.add(item)
        collect(value)
        return self._project(value)

    def _project(self, value, key=""):
        field = _field(key)
        if field in _PRIVATE_LOGS:
            return "<private-log>"
        if field == "status" and isinstance(value, str) and "\n" in value:
            return "<private-working-tree-status>"
        if field in _PRIVATE_FIELDS:
            return "<redacted>"
        if field in _ID_FIELDS and isinstance(value, str):
            # Stable aliases retain correlations within this artifact without
            # exposing the protocol's original random identifiers.
            if value not in self.identifiers:
                self.identifiers[value] = "identifier-" + str(self.next_identifier)
                self.next_identifier += 1
            return self.identifiers[value]
        if field == "endpoint" and isinstance(value, str):
            return value if value in {"lan", "public", "cloudflare", "isolated-pion"} else "<endpoint>"
        if isinstance(value, dict):
            return {self.text(k): self._project(v, k) for k, v in value.items()}
        if isinstance(value, list):
            return [self._project(v, key) for v in value]
        if isinstance(value, str):
            return self.text(value)
        return value

def write_public(path, value):
    path = Path(path)
    path.write_text(json.dumps(PublicEvidence().project(value), indent=2) + "\n", encoding="utf-8", newline="\n")

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("private_input", type=Path)
    parser.add_argument("public_output", type=Path)
    args = parser.parse_args()
    if args.private_input.resolve() == args.public_output.resolve():
        parser.error("Keep private originals separate from published evidence")
    write_public(args.public_output, json.loads(args.private_input.read_text(encoding="utf-8-sig")))
