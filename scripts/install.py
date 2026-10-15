#!/usr/bin/env python3
"""Guided terminal installer for container and bare-metal Haunt deployments."""

from __future__ import annotations

import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import sys
import textwrap
import urllib.parse
import uuid
from dataclasses import dataclass


@dataclass(frozen=True)
class Field:
    key: str
    label: str
    description: str
    default: str = ""
    modes: tuple[str, ...] = ("container", "bare-metal")
    choices: tuple[str, ...] = ()
    secret: bool = False


SECTIONS: tuple[tuple[str, tuple[Field, ...]], ...] = (
    ("Identity and HTTP", (
        Field("HAUNT_NAME", "Community name", "Displayed to members and in clients.", "Haunt"),
        Field("HAUNT_PUBLIC_URL", "Public URL", "Exact browser origin members use. Public hosts require HTTPS."),
        Field("HAUNT_LISTEN_ADDR", "HTTP listen address", "Bare-metal API/client listener. Put public deployments behind HTTPS.", "127.0.0.1:8080", ("bare-metal",)),
        Field("HAUNT_DATA_DIR", "Data directory", "Persistent private configuration, attachments, keys, plugins, and updates.", "data", ("bare-metal",)),
        Field("HAUNT_CLIENT_ORIGINS", "Additional client origins", "Comma-separated exact browser origins; wildcards are not accepted."),
        Field("HAUNT_TRUSTED_PROXIES", "Trusted proxy CIDRs", "Only proxies in these specific CIDRs may supply forwarding headers."),
    )),
    ("Database and authentication", (
        Field("HAUNT_DATABASE_URL", "PostgreSQL URL", "Dedicated PostgreSQL connection. Blank in container mode uses its private database.", "", ("container", "bare-metal"), secret=True),
        Field("HAUNT_DB_MAX_CONNS", "Database connections", "Maximum application pool size; include this in PostgreSQL capacity planning.", "16"),
        Field("HAUNT_SESSION_DAYS", "Session lifetime (days)", "How long remembered sign-ins remain valid, from 1 to 365.", "30"),
        Field("HAUNT_AUTH_PARALLEL", "Parallel password checks", "Each Argon2id operation uses about 64 MiB; range 1 to 32.", "4"),
    )),
    ("Messages and files", (
        Field("HAUNT_TEXT_MAX_BYTES", "Text storage budget (bytes)", "Maximum retained text data before policy cleanup.", "256000000"),
        Field("HAUNT_TEXT_RETENTION_DAYS", "Text retention (days)", "Retain channel text for this many days.", "30"),
        Field("HAUNT_IMAGE_MAX_BYTES", "Image limit (bytes)", "Maximum size of one image upload.", "2097152"),
        Field("HAUNT_VIDEO_MAX_BYTES", "Video limit (bytes)", "Maximum size of one video upload.", "10485760"),
        Field("HAUNT_ATTACHMENT_MAX_BYTES", "Attachment limit (bytes)", "Maximum size of one attachment; must cover image/video limits.", "1073741824"),
        Field("HAUNT_ATTACHMENT_RETENTION_DAYS", "Attachment retention (days)", "Retain message attachments for this many days.", "7"),
        Field("HAUNT_FFPROBE", "ffprobe command", "Executable used to inspect uploaded media content.", "ffprobe", ("bare-metal",)),
    )),
    ("Calls and media", (
        Field("HAUNT_MEDIA_PUBLIC_IP", "SFU public IP", "Public IP mapped to this host. Leave blank only when direct UDP is not used."),
        Field("HAUNT_MEDIA_PORT_MIN", "SFU UDP first port", "First directly mapped UDP media port; use 0 with max 0 to disable.", "50000"),
        Field("HAUNT_MEDIA_PORT_MAX", "SFU UDP last port", "Last directly mapped UDP media port; NAT must preserve port numbers.", "50100"),
        Field("HAUNT_MEDIA_MAX_PEERS", "Total media peers", "Server-wide concurrent peer safety bound, from 2 to 4096.", "512"),
        Field("HAUNT_MEDIA_ROOM_LIMIT", "Peers per room", "Per-call participant limit, from 2 to 64.", "16"),
        Field("HAUNT_ICE_SERVERS", "Custom ICE servers (JSON)", "JSON array of STUN/TURN server objects supplied to clients."),
        Field("HAUNT_CLOUDFLARED_ENABLED", "Cloudflared transport", "Select tunnel-safe media behavior when the HTTP service uses cloudflared.", "false", choices=("true", "false")),
        Field("HAUNT_MEDIA_TURN_HOST", "External TURN host", "Optional external TURN DNS name or IP, without scheme or port."),
        Field("HAUNT_MEDIA_TURN_PORT", "External TURN UDP/TCP port", "Plain TURN port used with the external TURN host.", "3478"),
        Field("HAUNT_MEDIA_TURN_TLS_PORT", "External TURN TLS port", "Optional TLS TURN port; 0 disables it.", "0"),
        Field("HAUNT_MEDIA_TURN_USERNAME", "External TURN username", "Dedicated relay account sent to clients."),
        Field("HAUNT_MEDIA_TURN_PASSWORD", "External TURN password", "Secret for the dedicated relay account.", secret=True),
        Field("HAUNT_CLOUDFLARE_TURN_KEY_ID", "Cloudflare TURN key ID", "Alternative to an external TURN host: Realtime TURN key identifier."),
        Field("HAUNT_CLOUDFLARE_TURN_API_TOKEN", "Cloudflare TURN API token", "Secret used server-side to obtain temporary TURN credentials.", secret=True),
    )),
    ("Built-in TURN relay", (
        Field("HAUNT_RELAY_ENABLED", "Enable built-in TURN", "Run Haunt's operator-managed TURN listener.", "false", choices=("true", "false")),
        Field("HAUNT_RELAY_PUBLIC_IP", "Relay public IP", "Advertised unicast IP for the built-in TURN relay."),
        Field("HAUNT_RELAY_PUBLIC_HOST", "Relay public host", "Optional TLS hostname; defaults to the relay public IP."),
        Field("HAUNT_RELAY_LISTEN_ADDR", "Relay listen address", "IP and port for plain TURN.", "0.0.0.0:3478"),
        Field("HAUNT_RELAY_BIND_IP", "Relay bind IP", "Local address used for allocated relay sockets.", "0.0.0.0"),
        Field("HAUNT_RELAY_PORT_MIN", "Relay first allocation port", "First port in the TURN relay allocation range.", "49160"),
        Field("HAUNT_RELAY_PORT_MAX", "Relay last allocation port", "Last port in the TURN relay allocation range.", "49999"),
        Field("HAUNT_RELAY_MAX_ALLOCATIONS", "Relay allocation limit", "Maximum simultaneous TURN allocations.", "512"),
        Field("HAUNT_RELAY_CREDENTIAL_MINUTES", "Relay credential lifetime", "Temporary TURN credential validity in minutes.", "30"),
        Field("HAUNT_RELAY_TLS_ADDR", "Relay TLS listen address", "Optional TLS TURN IP:port; certificate and key are then required."),
        Field("HAUNT_RELAY_TLS_CERT", "Relay TLS certificate", "Path to the relay certificate chain."),
        Field("HAUNT_RELAY_TLS_KEY", "Relay TLS private key", "Path to the relay private key.", secret=True),
        Field("HAUNT_RELAY_ALLOWED_PEER_CIDRS", "Relay private peer CIDRs", "Specific private destinations TURN may reach; catch-all ranges are rejected."),
    )),
    ("Client delivery and plugins", (
        Field("HAUNT_CLIENT_UPDATE_MANIFEST_URL", "External client update manifest", "HTTPS JSON URL for desktop client updates."),
        Field("HAUNT_CLIENT_UPDATE_LOCAL", "Serve local client releases", "Publish verified releases imported into the data directory.", "false", choices=("true", "false")),
        Field("HAUNT_CLIENT_UPDATE_INSTALLERS", "Show local installers", "Offer platform installers when local release serving is enabled.", "false", choices=("true", "false")),
        Field("HAUNT_AUDIO_PLUGIN_GITHUB_TOKEN_FILE", "Private plugin token file", "Optional in-runtime path to a mounted GitHub contents-read token; the token is never copied.", "", secret=True),
        Field("HAUNT_DEFAULT_AUDIO_PLUGIN", "Preload RNNoise", "Seeds the bundled RNNoise noise-suppression plugin on a fresh server.", "true", choices=("true", "false")),
    )),
    ("Container networking and updates", (
        Field("HAUNT_IMAGE", "Container image", "Published image reference or an explicitly pinned version.", "ghcr.io/example-git/haunt:latest", ("container",)),
        Field("HAUNT_HTTP_PORT", "Host HTTP port", "Host port mapped to bundled Nginx on container TCP 80. The supplied image keeps its 127.0.0.1:8080 backend internal.", "80", ("container",)),
        Field("HAUNT_HTTPS_PORT", "Host HTTPS port", "Host port mapped to container TCP 443.", "443", ("container",)),
        Field("HAUNT_TLS_MODE", "HTTPS mode", "auto obtains Let's Encrypt; provided uses mounted files; proxy trusts an external terminator.", "auto", ("container",), ("auto", "provided", "proxy")),
        Field("HAUNT_ACME_EMAIL", "Let's Encrypt email", "Optional ACME account contact for automatic HTTPS.", "", ("container",)),
        Field("HAUNT_PROXY_CIDRS", "External proxy CIDRs", "Exact proxy/connector ranges trusted by bundled Nginx.", "", ("container",)),
        Field("HAUNT_APP_UPDATE_REPOSITORY", "Application update repository", "GitHub owner/repository used for coordinated application releases.", "", ("container",)),
        Field("HAUNT_APP_UPDATE_MANIFEST_URL", "Application update manifest", "Alternative HTTPS manifest URL; configure only one update source.", "", ("container",)),
        Field("HAUNT_APP_UPDATE_AUTO", "Automatic app updates", "Apply verified application updates automatically.", "false", ("container",), ("true", "false")),
        Field("HAUNT_APP_UPDATE_INTERVAL", "Update interval (seconds)", "Seconds between application update checks.", "21600", ("container",)),
        Field("HAUNT_APP_UPDATE_TOKEN_FILE", "Private release token file", "Container path to a mounted GitHub contents-read token.", "", ("container",), secret=True),
    )),
)

INTEGER_RANGES = {
    "HAUNT_DB_MAX_CONNS": (2, 1024), "HAUNT_SESSION_DAYS": (1, 365), "HAUNT_AUTH_PARALLEL": (1, 32),
    "HAUNT_TEXT_MAX_BYTES": (4096, 1 << 50), "HAUNT_TEXT_RETENTION_DAYS": (1, 36500),
    "HAUNT_IMAGE_MAX_BYTES": (1, 1 << 50), "HAUNT_VIDEO_MAX_BYTES": (1, 1 << 50),
    "HAUNT_ATTACHMENT_MAX_BYTES": (1, 1 << 50), "HAUNT_ATTACHMENT_RETENTION_DAYS": (1, 36500),
    "HAUNT_MEDIA_PORT_MIN": (0, 65535), "HAUNT_MEDIA_PORT_MAX": (0, 65535),
    "HAUNT_MEDIA_MAX_PEERS": (2, 4096), "HAUNT_MEDIA_ROOM_LIMIT": (2, 64),
    "HAUNT_MEDIA_TURN_PORT": (1, 65535), "HAUNT_MEDIA_TURN_TLS_PORT": (0, 65535),
    "HAUNT_RELAY_PORT_MIN": (1, 65534), "HAUNT_RELAY_PORT_MAX": (1, 65534),
    "HAUNT_RELAY_MAX_ALLOCATIONS": (1, 4096), "HAUNT_RELAY_CREDENTIAL_MINUTES": (1, 1440),
    "HAUNT_HTTP_PORT": (1, 65535), "HAUNT_HTTPS_PORT": (1, 65535), "HAUNT_APP_UPDATE_INTERVAL": (60, 31536000),
}


def fields_for(mode: str) -> list[tuple[str, Field]]:
    return [(section, field) for section, fields in SECTIONS for field in fields if mode in field.modes]


def validate(field: Field, value: str, required: bool = False) -> str | None:
    if "\n" in value or "\r" in value or "\0" in value:
        return "Values must fit on one line."
    if required and not value.strip():
        return "A value is required."
    if not value:
        return None
    if field.choices and value not in field.choices:
        return "Choose one of: " + ", ".join(field.choices)
    if field.key in INTEGER_RANGES:
        low, high = INTEGER_RANGES[field.key]
        try:
            number = int(value)
        except ValueError:
            return f"Enter an integer from {low} to {high}."
        if number < low or number > high:
            return f"Enter an integer from {low} to {high}."
    if field.key == "HAUNT_PUBLIC_URL":
        parsed = urllib.parse.urlsplit(value)
        if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.path not in ("", "/") or parsed.query or parsed.fragment:
            return "Enter an HTTP(S) origin without credentials, path, query, or fragment."
        try:
            local = parsed.hostname == "localhost" or ipaddress.ip_address(parsed.hostname).is_private
        except ValueError:
            local = False
        if parsed.scheme != "https" and not local:
            return "Public hostnames require HTTPS."
    if field.key == "HAUNT_MEDIA_PUBLIC_IP":
        try:
            ipaddress.ip_address(value)
        except ValueError:
            return "Enter an IPv4 or IPv6 address."
    if field.key == "HAUNT_ICE_SERVERS":
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            return "Enter a valid JSON array."
        if not isinstance(parsed, list) or len(parsed) > 8:
            return "Enter a JSON array with at most eight servers."
    return None


def wrap(text: str, width: int = 72) -> str:
    return "\n".join(textwrap.wrap(text, width=width))


class Wizard:
    def __init__(self, stdin=sys.stdin, stdout=sys.stdout, *, color: bool | None = None):
        self.stdin, self.stdout = stdin, stdout
        self.color = stdout.isatty() if color is None else color
        self.values: dict[str, str] = {}
        self.mode = ""

    def paint(self, code: str, text: str) -> str:
        return f"\033[{code}m{text}\033[0m" if self.color else text

    def screen(self, step: str, title: str, body: str) -> None:
        if self.color:
            self.stdout.write("\033[2J\033[H")
        self.stdout.write(self.paint("1;35", "  HAUNT TERMINAL INSTALLER") + f"   {step}\n")
        self.stdout.write(self.paint("1", f"\n  {title}\n"))
        self.stdout.write("  " + "─" * 72 + "\n")
        for line in body.splitlines():
            self.stdout.write(("  " + line if line else "") + "\n")
        self.stdout.flush()

    def ask(self, prompt: str, default: str = "", *, secret: bool = False) -> str:
        suffix = f" [{default}]" if default else ""
        self.stdout.write(f"\n  {prompt}{suffix}: ")
        self.stdout.flush()
        if secret and self.stdin is sys.stdin and self.stdin.isatty():
            value = getpass.getpass("", stream=self.stdout)
        else:
            value = self.stdin.readline()
            if value == "":
                raise EOFError("installer input ended")
            value = value.rstrip("\r\n")
        return value if value else default

    def choose(self, prompt: str, options: tuple[str, ...], default: int = 0) -> str:
        for index, option in enumerate(options, 1):
            self.stdout.write(f"\n    {index}. {option}")
        while True:
            raw = self.ask(prompt, str(default + 1))
            if raw.isdigit() and 1 <= int(raw) <= len(options):
                return options[int(raw) - 1]
            self.stdout.write("\n  Choose a listed number.\n")

    def ask_field(self, field: Field, *, required: bool = False) -> str:
        current = self.values.get(field.key, field.default)
        while True:
            self.screen("CONFIGURE", field.label, wrap(field.description) + f"\n\nSetting: {field.key}")
            if field.choices:
                choice = self.choose("Choice", field.choices, field.choices.index(current) if current in field.choices else 0)
                return choice
            value = self.ask("Value", current, secret=field.secret)
            error = validate(field, value, required)
            if not error:
                return value
            self.stdout.write("\n  " + self.paint("31", error) + "\n")
            self.ask("Press Enter to try again")

    def run(self) -> tuple[str, dict[str, str], str, str]:
        self.screen("1 / 7", "Welcome", wrap("This guided installer prepares either the all-in-one container or a bare-metal server. Every setting remains available in the review screen. Nothing is written until you approve the summary."))
        self.ask("Press Enter to begin")

        self.screen("2 / 7", "Choose a deployment", "1. Container (recommended)\n   Bundled PostgreSQL, Nginx, web client, and automatic HTTPS.\n   Container TCP 80/443 maps to host TCP 80/443 by default; 8080 stays internal.\n\n2. Bare metal\n   You operate PostgreSQL, TLS, the service manager, and dependencies.")
        picked = self.choose("Deployment", ("Container", "Bare metal"))
        self.mode = "container" if picked == "Container" else "bare-metal"
        defaults = {f.key: f.default for _, f in fields_for(self.mode) if f.default != ""}
        self.values.update(defaults)
        self.values["HAUNT_INSTANCE_ID"] = str(uuid.uuid4())

        public = next(f for _, f in fields_for(self.mode) if f.key == "HAUNT_PUBLIC_URL")
        name = next(f for _, f in fields_for(self.mode) if f.key == "HAUNT_NAME")
        self.values[name.key] = self.ask_field(name, required=True)
        self.values[public.key] = self.ask_field(public, required=True)
        if self.mode == "container":
            media = next(f for _, f in fields_for(self.mode) if f.key == "HAUNT_MEDIA_PUBLIC_IP")
            self.values[media.key] = self.ask_field(media)
        else:
            database = next(f for _, f in fields_for(self.mode) if f.key == "HAUNT_DATABASE_URL")
            self.values[database.key] = self.ask_field(database, required=True)

        self.screen("4 / 7", "Create the first owner", wrap("These credentials create the first owner during startup, so this route skips the browser wizard. The password is written only to a separate mode-0600 file. It is never placed in the generated environment file."))
        while True:
            username = self.ask("Owner username", "owner").lower()
            if re.fullmatch(r"[a-z0-9_]{3,32}", username):
                break
            self.stdout.write("\n  Use 3–32 ASCII letters, digits, or underscores.\n")
        while True:
            password = self.ask("Owner password", secret=True)
            confirm = self.ask("Confirm password", secret=True)
            if len(password.encode()) >= 8 and len(password.encode()) <= 1024 and password == confirm:
                break
            self.stdout.write("\n  Passwords must match and contain 8–1024 UTF-8 bytes.\n")

        self.screen("5 / 7", "Default noise suppression", wrap("RNNoise is Haunt's bundled noise-suppression plugin. It is downloaded to clients and processes microphone audio locally. Preloading makes it available immediately; you can still remove or replace it later."))
        rnnoise = self.choose("Preload the default RNNoise plugin?", ("Yes", "No"))
        self.values["HAUNT_DEFAULT_AUDIO_PLUGIN"] = "true" if rnnoise == "Yes" else "false"

        available = fields_for(self.mode)
        page, page_size = 0, 12
        while True:
            pages = (len(available) + page_size - 1) // page_size
            rows = []
            start = page * page_size
            for index, (section, field) in enumerate(available[start:start + page_size], start + 1):
                value = self.values.get(field.key, "")
                shown = "••••••••" if field.secret and value else (value or "(unset)")
                rows.append(f"{index:>2}. {field.label:<31} {shown[:35]}")
            navigation = "N next" if page + 1 < pages else ""
            if page:
                navigation = (navigation + "  P previous").strip()
            self.screen("6 / 7", f"Review every setting — page {page + 1} / {pages}", "Enter a setting number to read its explanation and edit it.\n\n" + "\n".join(rows) + f"\n\n{navigation}  A approve")
            raw = self.ask("Choice", "A" if page + 1 == pages else "N")
            if raw.lower() == "a":
                break
            if raw.lower() == "n" and page + 1 < pages:
                page += 1
                continue
            if raw.lower() == "p" and page:
                page -= 1
                continue
            if raw.isdigit() and 1 <= int(raw) <= len(available):
                _, field = available[int(raw) - 1]
                self.values[field.key] = self.ask_field(field, required=field.key in ("HAUNT_PUBLIC_URL", "HAUNT_DATABASE_URL") and self.mode == "bare-metal")

        errors = validate_profile(self.mode, self.values)
        if errors:
            raise ValueError("; ".join(errors))
        self.screen("7 / 7", "Ready to write", f"Deployment: {self.mode}\nCommunity:  {self.values['HAUNT_NAME']}\nPublic URL: {self.values['HAUNT_PUBLIC_URL']}\nOwner:      {username}\nRNNoise:    {'preload' if self.values['HAUNT_DEFAULT_AUDIO_PLUGIN'] == 'true' else 'do not preload'}\n\nThe browser wizard will be skipped because this profile explicitly creates the first owner.")
        if self.choose("Write the installer files?", ("Yes", "Go back")) != "Yes":
            raise RuntimeError("installation cancelled")
        return self.mode, self.values, username, password


def validate_profile(mode: str, values: dict[str, str]) -> list[str]:
    errors = []
    lookup = {field.key: field for _, field in fields_for(mode)}
    for key, value in values.items():
        if key in lookup and (error := validate(lookup[key], value)):
            errors.append(f"{key}: {error}")
    try:
        media_min, media_max = int(values.get("HAUNT_MEDIA_PORT_MIN", "0")), int(values.get("HAUNT_MEDIA_PORT_MAX", "0"))
        if (media_min == 0) != (media_max == 0):
            errors.append("both media port bounds must be zero or nonzero")
        if media_min > media_max:
            errors.append("media port maximum must be at least its minimum")
    except ValueError:
        pass
    try:
        attachment = int(values.get("HAUNT_ATTACHMENT_MAX_BYTES", "1"))
        if int(values.get("HAUNT_IMAGE_MAX_BYTES", "1")) > attachment or int(values.get("HAUNT_VIDEO_MAX_BYTES", "1")) > attachment:
            errors.append("attachment limit must cover image and video limits")
    except ValueError:
        pass
    if values.get("HAUNT_MEDIA_TURN_HOST") and values.get("HAUNT_CLOUDFLARE_TURN_KEY_ID"):
        errors.append("choose external TURN or Cloudflare TURN, not both")
    if mode == "container" and values.get("HAUNT_APP_UPDATE_REPOSITORY") and values.get("HAUNT_APP_UPDATE_MANIFEST_URL"):
        errors.append("choose one application update source")
    return errors


def dotenv(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"').replace("$", "$$") + '"'


def write_private(path: Path, content: str, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if path.exists():
        raise FileExistsError(f"refusing to overwrite {path}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    descriptor = os.open(path, flags, mode)
    with os.fdopen(descriptor, "w", encoding="utf-8") as file:
        file.write(content)


def environment_text(mode: str, values: dict[str, str], username: str, password_file: str) -> str:
    quote = dotenv if mode == "container" else shlex.quote
    lines = ["# Generated by Haunt's terminal installer. Keep this file private.", f"# Deployment: {mode}", f"HAUNT_BOOTSTRAP_USERNAME={quote(username)}", f"HAUNT_BOOTSTRAP_PASSWORD_FILE={quote(password_file)}", f"HAUNT_INSTANCE_ID={quote(values['HAUNT_INSTANCE_ID'])}"]
    for section, fields in SECTIONS:
        selected = [field for field in fields if mode in field.modes]
        if not selected:
            continue
        lines.extend(("", f"# {section}"))
        for field in selected:
            value = values.get(field.key, "")
            if field.key in ("HAUNT_MEDIA_TURN_PORT", "HAUNT_MEDIA_TURN_TLS_PORT") and not values.get("HAUNT_MEDIA_TURN_HOST"):
                continue
            if not value:
                continue
            lines.append("# " + field.description)
            lines.append(f"{field.key}={quote(value)}")
    return "\n".join(lines) + "\n"


def write_install(mode: str, values: dict[str, str], username: str, password: str, output: Path, binary: str) -> list[str]:
    output = output.resolve()
    if output.exists() and any(output.iterdir()):
        raise FileExistsError(f"refusing to write into non-empty directory {output}")
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    password_path = output / "owner-password"
    write_private(password_path, password + "\n")
    env_path = output / ("container.env" if mode == "container" else "haunt.env")
    mounted_password = "/run/secrets/haunt-owner-password" if mode == "container" else str(password_path)
    write_private(env_path, environment_text(mode, values, username, mounted_password))
    created = [str(env_path), str(password_path)]
    if mode == "container":
        overlay = output / "compose.installer.yml"
        content = "services:\n  haunt:\n    volumes:\n      - type: bind\n        source: " + json.dumps(str(password_path)) + "\n        target: /run/secrets/haunt-owner-password\n        read_only: true\n"
        write_private(overlay, content)
        created.append(str(overlay))
    else:
        launcher = output / "run-haunt.sh"
        content = "#!/bin/sh\nset -eu\ninstaller_dir=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\nset -a\n. \"$installer_dir/haunt.env\"\nset +a\nexec " + shlex.quote(binary) + " \"$@\"\n"
        write_private(launcher, content, 0o700)
        created.append(str(launcher))
    return created


def demo_screens(directory: Path) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    samples = (
        ("01-welcome", "1 / 7", "Welcome", "This guided installer prepares either the all-in-one container or a bare-metal server. Every setting remains available in the review screen. Nothing is written until you approve the summary."),
        ("02-deployment", "2 / 7", "Choose a deployment", "1. Container (recommended)\n   Bundled PostgreSQL, Nginx, web client, and automatic HTTPS.\n   Container TCP 80/443 maps to host TCP 80/443 by default; 8080 stays internal.\n\n2. Bare metal\n   You operate PostgreSQL, TLS, the service manager, and dependencies."),
        ("03a-community-name", "CONFIGURE", "Community name", "Displayed to members and in clients.\n\nSetting: HAUNT_NAME\n\nValue [Haunt]:"),
        ("03b-public-url", "CONFIGURE", "Public URL", "Exact browser origin members use. Public hosts require HTTPS.\n\nSetting: HAUNT_PUBLIC_URL\n\nValue:"),
        ("03c-container-media", "CONFIGURE", "SFU public IP", "Public IP mapped to this host. Leave blank only when direct UDP is not used.\n\nSetting: HAUNT_MEDIA_PUBLIC_IP\n\nValue:"),
        ("03d-bare-database", "CONFIGURE", "PostgreSQL URL", "Dedicated PostgreSQL connection. Blank in container mode uses its private database.\n\nSetting: HAUNT_DATABASE_URL\n\nValue:"),
        ("04-owner", "4 / 7", "Create the first owner", "Owner username: owner\nOwner password: ••••••••••••\nConfirm password: ••••••••••••\n\nThe password is saved only in a private mode-0600 file."),
        ("05-rnnoise", "5 / 7", "Default noise suppression", "RNNoise is Haunt's bundled noise-suppression plugin. It processes microphone audio locally in each client.\n\nPreload the default RNNoise plugin?\n\n  1. Yes\n  2. No"),
        ("07-field", "CONFIGURE", "Peers per room", "Per-call participant limit, from 2 to 64.\n\nSetting: HAUNT_MEDIA_ROOM_LIMIT\n\nValue [16]:"),
        ("08-summary", "7 / 7", "Ready to write", "Deployment: container\nCommunity:  Night Owls\nPublic URL: https://haunt.example.com\nOwner:      owner\nRNNoise:    preload\n\nThe browser wizard will be skipped because this profile explicitly creates the first owner.\n\n  1. Yes\n  2. Go back"),
        ("09-finished", "COMPLETE", "Installer files created", "Private configuration and owner-password files are ready.\n\nStart the container with the generated Compose overlay. On its first start Haunt creates the owner, preloads RNNoise, and opens the normal client instead of the browser setup wizard."),
    )
    samples = list(samples)
    demo_values = {field.key: field.default for _, field in fields_for("container") if field.default}
    demo_values.update({"HAUNT_NAME": "Night Owls", "HAUNT_PUBLIC_URL": "https://haunt.example.com", "HAUNT_MEDIA_PUBLIC_IP": "198.51.100.10", "HAUNT_DEFAULT_AUDIO_PLUGIN": "true"})
    available, page_size = fields_for("container"), 12
    pages = (len(available) + page_size - 1) // page_size
    review_samples = []
    for page in range(pages):
        rows = []
        start = page * page_size
        for index, (_, field) in enumerate(available[start:start + page_size], start + 1):
            value = demo_values.get(field.key, "")
            shown = "••••••••" if field.secret and value else (value or "(unset)")
            rows.append(f"{index:>2}. {field.label:<31} {shown[:35]}")
        review_samples.append((f"06-settings-{page + 1}", "6 / 7", f"Review every setting — page {page + 1} / {pages}", "Enter a setting number to read its explanation and edit it.\n\n" + "\n".join(rows)))
    samples[8:8] = review_samples
    for name, step, title, body in samples:
        target = directory / f"{name}.txt"
        with target.open("w", encoding="utf-8") as output:
            Wizard(stdout=output, color=False).screen(step, title, body)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=Path("haunt-install"), help="new directory for private installer files")
    parser.add_argument("--binary", default="./haunt", help="bare-metal Haunt binary used by the generated launcher")
    parser.add_argument("--no-color", action="store_true")
    parser.add_argument("--demo-screens", type=Path, help="write representative screen captures as text and exit")
    args = parser.parse_args(argv)
    if args.demo_screens:
        demo_screens(args.demo_screens)
        return 0
    try:
        wizard = Wizard(color=False if args.no_color else None)
        mode, values, username, password = wizard.run()
        created = write_install(mode, values, username, password, args.output_dir, args.binary)
    except (EOFError, ValueError, RuntimeError, FileExistsError) as error:
        print(f"haunt installer: {error}", file=sys.stderr)
        return 1
    wizard.screen("COMPLETE", "Installer files created", "\n".join(created))
    if mode == "container":
        env_path = args.output_dir / "container.env"
        print(f"\n  Start with:\n  HAUNT_ENV_FILE={shlex.quote(str(env_path))} docker compose --env-file {shlex.quote(str(env_path))} -f compose.yml -f {shlex.quote(str(args.output_dir / 'compose.installer.yml'))} up -d")
    else:
        print(f"\n  Start with:\n  {args.output_dir / 'run-haunt.sh'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
