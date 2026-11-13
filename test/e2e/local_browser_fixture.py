"""Isolated server and secret-safe artifacts for local browser acceptance.

E2E standard: test/e2e/browser-e2e-testing-standard.md
No GPU, container runtime, management mock, or existing server is used.
"""

from __future__ import annotations

import fcntl
import hashlib
import html
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import threading
import urllib.error
import urllib.parse
import urllib.request
import zipfile


ROOT = Path(__file__).resolve().parents[2]
# The one admin password an isolated server is started with. Cases that log in
# through the API or a second browser context import this instead of spelling the
# literal again: six of them still said "password" and got HTTP 401 once the real
# value changed, which read like a product defect and was not one.
ADMIN_PASSWORD = "gpuworks-test-password-2026"


def digest(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def http(base: str, method: str, path: str, token: str = "", body=None):
    """A fresh transport with no cookie jar: bearer tests cannot inherit admin."""
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(base + path, method=method, headers=headers,
        data=None if body is None else json.dumps(body).encode())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        response = opener.open(request, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else None


def write_server_config(work: Path, storage_root: Path, listen: str, *, auto_check_updates=None,
                        session_idle_timeout=None) -> Path:
    """server.yaml that makes the isolated server a self-contained installation.

    The product refuses user-configured HTTP sources on private or loopback
    addresses unless outbound_allow_private_hosts lists them. The isolated
    server therefore allows exactly 127.0.0.1, where the audited local
    fixtures listen; every other private destination stays refused as in a
    default installation. storage_probe_roots is set to storage_root
    (private/storage) only so the startup check of existing storage has a
    root to compare against; it no longer limits which paths a storage may
    use. The seeded default storage keeps the product's /opt/gpuworks/models
    path, which is outside this root. listen is
    written instead of passing --addr so the server reports a consistent,
    non-overridden listen configuration. With --config the server also treats
    the private directory as its install directory: listen and update settings
    become editable and only ever rewrite this file.
    auto_check_updates optionally sets the product's per-installation switch;
    None leaves the existing configuration bytes and default behavior intact.
    session_idle_timeout optionally writes the product's own
    session_idle_timeout key, a Go duration such as "2s". It is the only way to
    reach an inactivity deadline inside a test window, since the shipped
    default in auth.SessionPolicy.Normalized is 24h. None writes no key at all,
    so every existing caller keeps byte-identical configuration and the
    product's 24h default.
    """
    if auto_check_updates is not None and type(auto_check_updates) is not bool:
        raise ValueError('auto_check_updates must be bool or None')
    if session_idle_timeout is not None:
        if not isinstance(session_idle_timeout, str) or not session_idle_timeout.strip():
            raise ValueError('session_idle_timeout must be a Go duration string such as "2s"')
        # The product's parser accepts a bare Go duration; a value it silently
        # ignored would leave the 24h default in place and make an expiry test
        # pass for the wrong reason, so reject anything the shape check doubts.
        if not re.fullmatch(r"[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)", session_idle_timeout.strip()):
            raise ValueError(f'session_idle_timeout is not a Go duration: {session_idle_timeout!r}')

    def quoted(value: str) -> str:
        # Go's YAML decoder rejects raw DEL/C1 controls and the surrogate-pair
        # escapes json.dumps emits for non-BMP text; printable text written
        # unescaped is a valid double-quoted scalar.
        if not value.isprintable():
            raise ValueError(f"server.yaml value must be printable: {value!r}")
        return json.dumps(value, ensure_ascii=False)

    config = work / "server.yaml"
    text = "\n".join(["listen:", "  console:", "    - " + quoted(listen),
                      "storage_probe_roots:", "  - " + quoted(str(storage_root)),
                      "outbound_allow_private_hosts:", "  - " + quoted("127.0.0.1"), ""])
    if auto_check_updates is not None:
        text += "auto_check_updates: " + str(auto_check_updates).lower() + "\n"
    if session_idle_timeout is not None:
        text += "session_idle_timeout: " + quoted(session_idle_timeout.strip()) + "\n"
    with os.fdopen(os.open(config, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as handle:
        handle.write(text)
    return config


class LocalServer:
    def __init__(self, binary: Path, work: Path, *, env_overrides=None, auto_check_updates=None,
                 session_idle_timeout=None):
        # Resolved once: storage roots compare lexically cleaned paths, so cases
        # must build their paths from the same spelling the root uses.
        self.binary, self.work = binary.resolve(), work.resolve()
        self.env_overrides = dict(env_overrides or {})
        self.auto_check_updates = auto_check_updates
        # None keeps the product's own 24h inactivity default and writes no key.
        self.session_idle_timeout = session_idle_timeout
        # The only storage root; cases put every storage path below it.
        self.storage_root = self.work / "storage"
        self.environment = {}
        self.process = None
        self.port_lock = None
        self.reader = None
        self.base = ""

    def missing_binary_hint(self) -> str:
        """A sentence naming what to run, for the preflight message.

        Each case reports BLOCKED_INFRA when a prerequisite is absent, but the
        traceback alone leaves a newcomer guessing. This names the command.
        """
        return (f"{self.binary} does not exist. Build the binaries first with `make build`, "
                f"which produces bin/gpuworks and bin/gpuworks-agent, or point the case at an "
                f"existing build (--server-bin / --agent-bin). "
                f"See test/e2e/RUNNING.md for the full prerequisite list.")

    def require_binary(self):
        if not self.binary.exists():
            raise FileNotFoundError(self.missing_binary_hint())

    def start(self):
        self.require_binary()
        # Cross-process locks close the bind/start race for concurrent cases.
        for port in range(8800, 8900, 2):
            lock = open(Path(tempfile.gettempdir()) / f"gpuworks-e2e-{os.getuid()}-{port}.lock", "a")
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with socket.socket() as probe:
                    probe.bind(("127.0.0.1", port))
            except (BlockingIOError, OSError):
                lock.close()
                continue
            self.port_lock = lock
            self.base = f"http://127.0.0.1:{port}"
            break
        if not self.port_lock:
            raise RuntimeError("no available isolated server port (8800..8898, step 2)")
        data = self.work / "server-data"
        data.mkdir(mode=0o700)
        credentials = data / "initial-credentials"
        credentials.write_text(json.dumps({"user": "admin", "password": ADMIN_PASSWORD}))
        credentials.chmod(0o600)
        self.storage_root.mkdir(mode=0o700)
        config = write_server_config(self.work, self.storage_root, self.base.removeprefix("http://"),
                                     auto_check_updates=self.auto_check_updates,
                                     session_idle_timeout=self.session_idle_timeout)
        self.environment = {"config": "private/server.yaml", "listen": [self.base.removeprefix("http://")],
                            "storage_probe_roots": [str(self.storage_root)],
                            "outbound_allow_private_hosts": ["127.0.0.1"]}
        child_env = {**os.environ, "GPUWORKS_ADMIN_PASSWORD": ADMIN_PASSWORD}
        for name, value in self.env_overrides.items():
            if value is None:
                child_env.pop(name, None)
            else:
                child_env[name] = value
        self.process = subprocess.Popen([str(self.binary), "serve", "--config", str(config),
            "--data", str(data), "--log-level", "info"],
            cwd=ROOT, env=child_env,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        ready = threading.Event()
        listening = []

        def collect():
            with (self.work / "server.log").open("w") as log:
                for line in self.process.stdout:
                    log.write(line)
                    log.flush()
                    if 'msg="server listening"' in line:
                        listening.append(True)
                        ready.set()
            ready.set()  # EOF also wakes the caller, which checks listening.

        self.reader = threading.Thread(target=collect, daemon=True)
        self.reader.start()
        if not ready.wait(40) or not listening or self.process.poll() is not None:
            raise RuntimeError("isolated server failed readiness; inspect private/server.log")
        code, health = http(self.base, "GET", "/healthz")
        if code != 200:
            raise RuntimeError(f"isolated server health returned {code}")
        return health

    def stop(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        if self.reader:
            self.reader.join(timeout=5)
        if self.port_lock:
            self.port_lock.close()
        return self.process is None or self.process.poll() is not None


class Artifacts:
    def __init__(self, directory: Path):
        self.directory = directory.resolve()
        self.directory.mkdir(parents=True, exist_ok=False, mode=0o700)
        self.private = self.directory / "private"
        self.private.mkdir(mode=0o700)
        self.secrets = ["gpuworks-test-password-2026"]
        self.tracing = False
        self.trace_number = 0

    def remember(self, value):
        if value and value not in self.secrets:
            self.secrets.append(value)

    def _secret_variants(self, value: str):
        """Yield the raw, escaped and encoded forms a secret can take on disk."""
        seen = set()
        candidates = [value, value.strip()]
        if "PRIVATE KEY-----" in value:
            # Browser diagnostics can truncate a fill argument to a payload
            # line, so matching only the complete PEM is insufficient.
            candidates.extend(line.strip() for line in value.splitlines()
                              if len(line.strip()) >= 16
                              and re.fullmatch(r"[A-Za-z0-9+/=]+", line.strip()))
        for candidate in candidates:
            if not candidate or candidate in seen:
                continue
            seen.add(candidate)
            yield candidate
            for encoded in (
                json.dumps(candidate, ensure_ascii=True)[1:-1],
                json.dumps(candidate, ensure_ascii=False)[1:-1],
                urllib.parse.quote(candidate, safe=""),
                html.escape(candidate),
            ):
                if encoded and encoded not in seen:
                    seen.add(encoded)
                    yield encoded

    def redact(self, text: str) -> str:
        # A multiline PEM becomes "\n..." inside JSON, so matching only the raw
        # value is not enough. Replace every escaped/encoded form as well.
        for value in sorted(self.secrets, key=len, reverse=True):
            for variant in self._secret_variants(value):
                text = text.replace(variant, "[REDACTED]")
        return re.sub(r"gwp_[A-Za-z0-9_-]{32,}", "[REDACTED_TOKEN]", text)

    def _redact_value(self, value):
        if isinstance(value, str):
            return self.redact(value)
        if isinstance(value, list):
            return [self._redact_value(item) for item in value]
        if isinstance(value, tuple):
            return tuple(self._redact_value(item) for item in value)
        if isinstance(value, dict):
            return {self._redact_value(key): self._redact_value(item)
                    for key, item in value.items()}
        return value

    def write_json(self, name: str, value):
        # Redact before json.dumps so a multiline secret never enters the
        # serialized form; the second pass catches escaped variants.
        cleaned = self._redact_value(value)
        text = json.dumps(cleaned, ensure_ascii=False, indent=2)
        (self.directory / name).write_text(self.redact(text) + "\n")

    def _contains_secret(self, text: str) -> bool:
        for secret in self.secrets:
            if len(secret) <= 8:
                continue
            if any(variant and variant in text for variant in self._secret_variants(secret)):
                return True
        return False

    def _scan_text(self, text: str) -> bool:
        if self._contains_secret(text):
            return True
        try:
            value = json.loads(text)
        except (json.JSONDecodeError, ValueError):
            return False
        pending = [value]
        while pending:
            item = pending.pop()
            if isinstance(item, str):
                if self._contains_secret(item):
                    return True
            elif isinstance(item, dict):
                pending.extend(item.keys())
                pending.extend(item.values())
            elif isinstance(item, list):
                pending.extend(item)
        return False

    def _scan_bytes(self, content: bytes) -> bool:
        # A binary body can contain UTF-8 secrets even when its surrounding
        # bytes cannot be decoded. Never transcode that body to scan it.
        return (any(variant.encode() in content for secret in self.secrets
                    if len(secret) > 8 for variant in self._secret_variants(secret))
                or self._scan_text(content.decode("utf-8", errors="ignore")))

    def start_trace(self, context):
        # Never call while login/one-time credentials are being displayed.
        context.tracing.start(screenshots=False, snapshots=True, sources=False)
        self.tracing = True

    def stop_trace(self, context):
        if not self.tracing:
            return
        self.trace_number += 1
        raw = self.private / "trace.zip"
        context.tracing.stop(path=raw)
        self.tracing = False
        for cookie in context.cookies():
            self.remember(cookie["value"])
        destination = self.directory / f"trace-{self.trace_number}.zip"
        self._rewrite_trace(raw, destination)
        raw.unlink()

    def _rewrite_trace(self, raw: Path, destination: Path):
        """Redact text resources and copy binary ones byte-for-byte.

        Decoding a PNG or font as latin-1 and re-encoding it as UTF-8 changes
        its bytes; binary resources are therefore scanned in memory and written
        unchanged.
        """
        pending = self.private / (destination.name + ".pending")
        try:
            with zipfile.ZipFile(raw) as source, zipfile.ZipFile(pending, "w", zipfile.ZIP_DEFLATED) as output:
                for entry in source.infolist():
                    content = source.read(entry)
                    try:
                        text = content.decode()
                    except UnicodeDecodeError:
                        text = None
                    if text is None or "\x00" in text:
                        if self._scan_bytes(content):
                            raise RuntimeError("credential scan rejected a trace resource")
                        output.writestr(entry, content)
                        continue
                    redacted = self.redact(text).encode()
                    if self._scan_bytes(redacted):
                        raise RuntimeError("credential scan rejected a trace resource")
                    output.writestr(entry, redacted)
            pending.replace(destination)
        except Exception:
            pending.unlink(missing_ok=True)
            raise

    def capture_failure(self, page, sensitive: bool) -> str:
        """Save a failure screenshot unless secret material may be visible.

        Cases handling private keys or one-time credentials pass sensitive=True
        for their whole run; no artifact may contain the secret even when an
        assertion about not echoing it fails.
        """
        if sensitive:
            return "omitted: secret material may be visible"
        page.screenshot(path=self.directory / "failure.png")
        return "failure.png"

    def scan(self):
        for path in self.directory.iterdir():
            if not path.is_file():
                continue
            if path.suffix == ".zip":
                with zipfile.ZipFile(path) as archive:
                    contents = [archive.read(name) for name in archive.namelist()]
            else:
                contents = [path.read_bytes()]
            for data in contents:
                if self._scan_bytes(data):
                    raise RuntimeError(f"credential scan rejected {path.name}")
