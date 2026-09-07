"""Core Cloudflare TempEmail deployment orchestration.

This module deliberately stops after D1/KV, Worker, and Pages are healthy. Email
Routing, DNS/MX, and real receive verification are separate later stages.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import quopri
import re
import secrets
import shutil
import string
import time
import subprocess
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence
from urllib.request import Request, urlopen

try:
    from .credential_ledger import (
        LedgerError,
        append_record,
        read_records,
        select_binding,
        select_credential,
    )
except ImportError:  # pragma: no cover - direct container entry point
    from credential_ledger import (  # type: ignore
        LedgerError,
        append_record,
        read_records,
        select_binding,
        select_credential,
    )


REQUIRED_CAPABILITIES = (
    frozenset(("account settings read",)),
    frozenset(("workers scripts write", "workers scripts edit")),
    frozenset(("workers routes write", "workers routes edit")),
    frozenset(("workers kv storage write", "workers kv storage edit")),
    frozenset(("d1 write", "d1 edit")),
    frozenset(("pages write", "pages edit")),
    frozenset(("zone read",)),
    frozenset(("dns write", "dns edit")),
    frozenset(("email routing rules write", "email routing rules edit")),
    frozenset(("email routing addresses write", "email routing addresses edit")),
    frozenset(("workers ai read",)),
    frozenset(("workers ai write", "workers ai edit")),
)
PROOFS = ("token", "account", "zone", "resource_read")
SOURCE_DIRS = ("worker", "frontend", "db")
ROOT_METADATA = ("pnpm-workspace.yaml", "pnpm-lock.yaml", "package.json", ".npmrc")
URL_RE = re.compile(r"https://[a-zA-Z0-9][a-zA-Z0-9._-]*(?:/[A-Za-z0-9._~:/?#@!$&'()*+,;=%-]*)?")


class DeploymentError(RuntimeError):
    """A deployment invariant or remote ownership check failed."""


class CommandError(DeploymentError):
    def __init__(self, exit_code: int, safe_output: str):
        self.exit_code = int(exit_code)
        self.safe_output = safe_output
        super().__init__(f"command failed with exit {self.exit_code}: {safe_output}")


def _curl_http(
    method: str,
    url: str,
    headers: Mapping[str, str] | None = None,
    data: bytes | None = None,
    timeout: float = 30,
) -> tuple[int, str]:
    """HTTP through the curl binary.

    Cloudflare applies browser-signature (error 1010) checks to Worker hostnames;
    Python's default TLS fingerprint is rejected while curl passes, so the deploy
    transport must not rely on urllib for Worker-hosted endpoints.
    """
    cmd = [
        "curl", "-sS", "--http2", "-m", str(max(1, int(timeout))),
        "--noproxy", "*", "--retry", "5", "--retry-delay", "2", "--retry-all-errors",
        "-X", method, "-o", "-", "-w", "\n%{http_code}",
    ]
    for key, value in (headers or {}).items():
        cmd.extend(["-H", f"{key}: {value}"])
    if data is not None:
        cmd.extend(["--data-binary", "@-"])
    cmd.append(url)
    try:
        proc = subprocess.run(
            cmd, input=data, capture_output=True, timeout=timeout + 10
        )
    except subprocess.TimeoutExpired as exc:
        raise DeploymentError("worker HTTP request timed out") from exc
    if proc.returncode != 0:
        raise DeploymentError(
            f"worker HTTP transport failed: {proc.stderr.decode('utf-8', 'replace').strip()[:200]}"
        )
    output = proc.stdout.decode("utf-8", "replace")
    code_text = output.rsplit("\n", 1)[-1].strip()
    body = output[: -len(code_text) - 1] if code_text.isdigit() else output
    try:
        status = int(code_text)
    except ValueError:
        status = 599
    return status, body


class WorkerHttpClient:
    """Small injectable Worker API client used by receive verification."""
    def __init__(self, base_url: str, http: Callable[..., Any] | None = None, domain: str | None = None):
        self.base_url = base_url.rstrip("/")
        self.http = http or self._request
        self.domain = domain

    def _request(self, method: str, url: str, **kwargs: Any) -> tuple[int, str]:
        data = kwargs.get("json")
        body = json.dumps(data).encode() if data is not None else None
        headers = {"Content-Type": "application/json", **kwargs.get("headers", {})}
        return _curl_http(
            method, url, headers=headers, data=body, timeout=kwargs.get("timeout", 20)
        )

    def _call(self, method: str, path: str, headers: Mapping[str, str], payload: Mapping[str, Any] | None = None) -> Mapping[str, Any]:
        attempts = 6
        last: DeploymentError | None = None
        for attempt in range(attempts):
            try:
                status, body = self.http(method, self.base_url + path, headers=dict(headers), json=payload)
            except DeploymentError as exc:
                # The container path to the freshly deployed Worker hostname can
                # flap for a short window (transient DNS / edge settle) right after
                # deploy. Retry transport-level failures like the health window does.
                text = str(exc).lower()
                if not any(k in text for k in ("resolve", "connect", "timed out", "couldn't connect", "couldn't resolve", "timeout")):
                    raise
                last = exc
                if attempt < attempts - 1:
                    time.sleep(3 + attempt)
                    continue
                raise last
            if status >= 400:
                raise DeploymentError(f"worker API HTTP {status}")
            try: result = json.loads(body) if isinstance(body, str) else body
            except json.JSONDecodeError as exc: raise DeploymentError("worker API returned invalid JSON") from exc
            if not isinstance(result, Mapping): raise DeploymentError("worker API response malformed")
            return result
        raise DeploymentError("worker API transport failed")


    def set_base_url(self, base_url: str) -> None:
        """Update the URL after Wrangler has created/reused the Worker."""
        self.base_url = str(base_url).rstrip("/")

    def new_address(self, admin_auth: str, random_subdomain: bool = False) -> Mapping[str, Any]:
        name = "verify-" + secrets.token_hex(8)
        domain = self.domain or self.base_url.split("//", 1)[-1].split("/", 1)[0]
        return self._call("POST", "/admin/new_address", {"x-admin-auth": admin_auth}, {"name": name, "domain": domain, "enableRandomSubdomain": random_subdomain})

    def send_mail(self, admin_auth: str, sender: str, recipient: str, marker: str) -> Mapping[str, Any]:
        # Generic admin send endpoint: resolves to the configured Resend / SMTP /
        # send_email-binding chain in the Worker instead of forcing the deprecated
        # Cloudflare Send Email binding alone.
        return self._call(
            "POST", "/admin/send_mail", {"x-admin-auth": admin_auth},
            {
                "from_name": "", "from_mail": sender,
                "to_mail": recipient, "to_name": "",
                "subject": marker, "content": marker, "is_html": False,
            },
        )

    def parsed_mails(self, jwt: str) -> Mapping[str, Any]:
        return self._call("GET", "/api/parsed_mails?limit=20&offset=0", {"Authorization": f"Bearer {jwt}"})

    def mails(self, jwt: str) -> Mapping[str, Any]:
        return self._call("GET", "/api/mails?limit=20&offset=0", {"Authorization": f"Bearer {jwt}"})

    def raw_mail(self, jwt: str, mail_id: Any) -> Mapping[str, Any]:
        return self._call("GET", f"/api/mail/{mail_id}", {"Authorization": f"Bearer {jwt}"})


def _default_smtp_sender(config: Mapping[str, Any], from_addr: str, recipient: str, marker: str, timeout: float = 30) -> None:
    """Send the receive marker via an external SMTP server.

    Cloudflare has retired its Send Email API (new accounts cannot send through
    the `SEND_MAIL` binding), so receive verification must be able to use an
    operator-provided SMTP relay instead of the Worker's outbound binding.
    """
    import smtplib
    from email.message import EmailMessage

    host = str(config.get("host") or "").strip()
    if not host:
        raise DeploymentError("smtp_config missing host")
    port = int(config.get("port", 587))
    use_starttls = bool(config.get("starttls", True))
    user = str(config.get("user") or "").strip()
    password = str(config.get("password") or "")
    sender = str(config.get("from") or from_addr or user or "").strip()
    msg = EmailMessage()
    msg.set_content(marker)
    msg["Subject"] = marker
    msg["From"] = sender
    msg["To"] = recipient
    smtp = smtplib.SMTP(host, port, timeout=timeout)
    try:
        smtp.ehlo_or_helo_if_needed()
        if use_starttls:
            smtp.starttls()
            smtp.ehlo_or_helo_if_needed()
        if user:
            smtp.login(user, password)
        smtp.sendmail(sender, [recipient], msg.as_string())
    finally:
        try:
            smtp.quit()
        except Exception:
            pass


def _load_smtp_config(credentials_root: Path) -> Mapping[str, Any] | None:
    path = Path(credentials_root) / "smtp_config.json"
    try:
        with open(path, encoding="utf-8") as fh:
            cfg = json.load(fh)
        if isinstance(cfg, Mapping) and cfg.get("host"):
            return cfg
    except (OSError, json.JSONDecodeError):
        pass
    return None


def _make_smtp_sender(config: Mapping[str, Any]) -> Callable[..., Any]:
    def send(from_addr: str, recipient: str, marker: str) -> None:
        _default_smtp_sender(config, from_addr, recipient, marker)
    return send


def verify_receive(worker: Any, deployment: Mapping[str, Any], credentials_root: Path, *, attempts: int | None = None, timeout: float = 60, sleep: Callable[[float], None] = time.sleep, marker: str | None = None, smtp_sender: Callable[..., Any] | None = None, prepare_send: Callable[[str, str, str], Any] | None = None) -> Mapping[str, Any]:
    started = _now(); marker = marker or "tempmail-verify-" + secrets.token_urlsafe(18)
    sender = str(deployment.get("sender") or ""); admin = str(deployment.get("admin_password") or "")
    result: dict[str, Any] = {"deployment_id": deployment.get("deployment_id"), "sender": sender, "recipient": "", "marker": marker, "started_at": started, "send_result": "", "poll_count": 0}
    status = "api_failed"; reason = ""
    try:
        worker_url = str(deployment.get("worker_url") or "").strip()
        if worker_url and hasattr(worker, "set_base_url"):
            worker.set_base_url(worker_url)
        base = worker.new_address(admin, False); random = worker.new_address(admin, True)
        recipient = str(random.get("address") or ""); jwt = str(random.get("jwt") or "")
        if not recipient or not jwt: raise DeploymentError("random recipient response missing address or credential")
        result["recipient"] = recipient
        result["jwt"] = jwt
        sender = str(base.get("address") or base.get("email") or "")
        result["sender"] = sender
        try:
            if prepare_send is not None:
                prepare_send(sender, recipient, jwt)
            if smtp_sender is not None:
                smtp_sender(sender, recipient, marker)
            else:
                sent = worker.send_mail(admin, sender, recipient, marker)
        except Exception as exc:
            raise DeploymentError(f"send failed: {exc}") from exc
        result["send_result"] = "ok"
        deadline = time.monotonic() + timeout
        i = 0
        while attempts is None or i < attempts:
            i += 1
            result["poll_count"] = i
            mails = worker.parsed_mails(jwt)
            rows = mails.get("results", [])
            for mail in rows if isinstance(rows, list) else []:
                blob = " ".join(str(mail.get(k, "")) for k in ("subject", "text", "html", "body"))
                norm = lambda x: str(x).strip().lower().strip("<>")
                if norm(sender) == norm(mail.get("sender", "")) and norm(recipient) == norm(mail.get("address", "")) and marker in blob:
                    status = "verified"; reason = ""; raise StopIteration
            if time.monotonic() >= deadline: break
            sleep(min(1.0, max(0, deadline - time.monotonic())))
        if status != "verified": status, reason = "timeout", "marker email not observed"
    except StopIteration:
        pass
    except DeploymentError as exc:
        status, reason = ("send_failed" if "send" in str(exc).lower() else "api_failed"), str(exc)
    except Exception as exc:
        status, reason = "api_failed", str(exc)
    result.update({"finished_at": _now(), "status": status, "reason": reason[:500]})
    append_record(credentials_root, "receive-verifications", result)
    return result


DEST_VERIFY_LINK_RE = re.compile(r"https://dash\.cloudflare\.com/email_fwdr/verify\?token=[A-Za-z0-9_\-.=]+")


def extract_destination_verify_link(raw_mail: str) -> str | None:
    """Extract the Cloudflare destination verification link from raw MIME."""
    body = quopri.decodestring(str(raw_mail or "").encode("utf-8", "ignore")).decode("utf-8", "ignore")
    # MIME producers often soft/hard-wrap right inside the auth token; tokens never
    # contain whitespace, so flatten before matching or the link glues to junk
    # ("Account="...) or comes out truncated (real 307-char URL with 255-char token).
    flat = re.sub(r"\s+", "", body)
    matches = DEST_VERIFY_LINK_RE.findall(flat)
    valid = [m for m in matches if len(m) >= 250]
    return valid[0] if valid else None


def _default_confirm_link(link: str, *, headless: bool = True) -> str:
    """Open the CF verification link in the stack CloakBrowser (sing-box -> WARP).

    Imported lazily so the deploy module can run host-side without playwright.
    Returns a page-text snippet for success detection.
    """
    try:
        from geber_cf import _launch_browser_context
    except ImportError as exc:  # pragma: no cover - direct container path
        raise DeploymentError("geber_cf CloakBrowser unavailable for destination confirmation") from exc
    ctx, page = _launch_browser_context(headless=headless, tag="[deploy-verify]")
    try:
        try:
            page.goto(link, wait_until="domcontentloaded", timeout=90000)
        except Exception:
            pass
        try:
            page.wait_for_timeout(4000)
        except Exception:
            pass
        try:
            snippet = (page.inner_text("body") or "")[:400]
        except Exception:
            snippet = ""
    finally:
        ctx.close()
    return str(snippet)


def ensure_send_destination_verified(
    client: Any, *, account_id: str, verify_address: str, verify_jwt: str,
    worker: Any,
    link_confirmer: Callable[[str], str] | None = None,
    mail_poll_timeout: float = 150.0, confirm_attempts: int = 3,
    sleep: Callable[[float], None] = time.sleep,
) -> Mapping[str, Any]:
    """Run the new-account send_email destination-address verification loop.

    Fresh Cloudflare accounts refuse send_email deliveries to unknown recipients.
    Registering the local recipient as an Email Routing destination address, then
    click-confirming the emailed verify link, flips the destination to verified and
    unblocks the SEND_MAIL binding for that address.
    """
    if hasattr(client, "list_email_routing_destination_addresses") and hasattr(client, "create_email_routing_destination_address"):
        existing = client.list_email_routing_destination_addresses(account_id)
        target = verify_address.strip().lower()
        if any(str(row.get("email") or "").strip().lower() == target
               and str(row.get("status") or "").strip().lower() == "verified" for row in existing):
            return {"mode": "preverified", "confirmed": True, "detail": "destination already verified"}
        if not any(str(row.get("email") or "").strip().lower() == target for row in existing):
            client.create_email_routing_destination_address(account_id, verify_address)

    confirm = link_confirmer or _default_confirm_link
    deadline = time.monotonic() + mail_poll_timeout
    link: str | None = None
    while time.monotonic() < deadline and not link:
        listing = worker.mails(verify_jwt) if hasattr(worker, "mails") else {}
        rows = listing.get("results", []) if isinstance(listing, Mapping) else []
        for mail in rows if isinstance(rows, list) else []:
            mail_id = mail.get("id") if isinstance(mail, Mapping) else None
            if mail_id is None:
                continue
            detail = worker.raw_mail(verify_jwt, mail_id)
            link = extract_destination_verify_link(str(detail.get("raw") or ""))
            if link:
                break
        if not link:
            sleep(5.0)
    if not link:
        raise DeploymentError("destination verification email not observed")
    detail = _send_confirm_loop(confirm, link, attempts=confirm_attempts, sleep=sleep)
    account_status = _destination_status(client, account_id=account_id, email=verify_address)
    confirmed = "verified" in detail.lower() or account_status == "verified"
    return {"mode": "send_mail_binding", "confirmed": confirmed,
            "account_status": account_status, "detail": detail[:200]}


def _send_confirm_loop(confirm: Callable[[str], str], link: str, *, attempts: int, sleep: Callable[[float], None]) -> str:
    detail = ""
    for _ in range(attempts):
        try:
            detail = confirm(link) or ""
            if "verified" in detail.lower():
                break
        except Exception as exc:
            detail = str(exc)
        sleep(4.0)
    return detail


def _destination_status(client: Any, *, account_id: str, email: str) -> str:
    if not hasattr(client, "list_email_routing_destination_addresses"):
        return "unknown"
    rows = client.list_email_routing_destination_addresses(account_id)
    target = email.strip().lower()
    for row in rows:
        if str(row.get("email") or "").strip().lower() == target:
            return str(row.get("status") or "").strip().lower()
    return "missing"


def _api_call(client: Any, names: Sequence[str], *args: Any, **kwargs: Any) -> Any:
    for name in names:
        method = getattr(client, name, None)
        if method is not None:
            result = method(*args, **kwargs)
            if isinstance(result, Mapping) and "success" in result and "result" in result:
                if not result.get("success"):
                    errors = result.get("errors") or "Cloudflare API request failed"
                    raise DeploymentError(str(errors)[:500])
                return result.get("result")
            return result
    raise DeploymentError(f"API client lacks required operation: {names[0]}")


def wildcard_mx_payloads(records: Sequence[Mapping[str, Any]], domain: str) -> list[dict[str, Any]]:
    """Copy every apex MX target/priority to wildcard, normalizing only name."""
    apex = domain.strip().rstrip(".").lower()
    out: list[dict[str, Any]] = []
    seen: set[tuple[Any, Any]] = set()
    for row in records:
        name = str(row.get("name", "")).strip().rstrip(".").lower()
        if name not in (apex, "@") or str(row.get("type", "MX")).upper() != "MX":
            continue
        if "content" not in row and "target" not in row:
            raise DeploymentError("apex MX record lacks target")
        item = {"type": "MX", "name": f"*.{apex}", "ttl": row.get("ttl", 300), "priority": row.get("priority")}
        item["content"] = row.get("content", row.get("target"))
        key = (item["content"], item.get("priority"))
        if key not in seen:
            seen.add(key); out.append(item)
    if not out:
        raise DeploymentError(f"no apex MX records found for {apex}")
    return out


def _permission_limited(exc: Exception) -> bool:
    """True when Cloudflare rejects a call because the token lacks the permission group."""
    text = str(exc)
    codes = ("10000", "9109", "9106", "6003", "7000")
    return any(
        f'"code":{code}' in text or f'"code": {code}' in text
        for code in codes
    ) or (
        "permission" in text.lower() and "denied" in text.lower()
    )


def _dash_request(cookies, method: str, path: str, body=None):
    """Issue a request to the Cloudflare dashboard API using saved dash cookies."""
    from playwright.sync_api import sync_playwright
    with sync_playwright() as p:
        ctx = p.request.new_context()
        for c in cookies:
            try:
                ctx.add_cookies([{k: c[k] for k in ("name", "value", "domain", "path")}])  # noqa: E501
            except Exception:
                continue
        opt = {"method": method}
        if body is not None:
            opt["data"] = json.dumps(body)
            opt["headers"] = {"Content-Type": "application/json"}
        r = ctx.request.fetch("https://dash.cloudflare.com/api/v4" + path, **opt)
        try:
            result = (r.status, r.json())
        except Exception:
            result = (r.status, None)
    return result


def _find_dash_cookie_for_zone(output_root, zone_id: str):
    """Find a saved Phase 1 dash cookie that owns the given zone."""
    if not output_root:
        return None
    for cf in sorted(Path(output_root).glob("cf_cookies_*.json")):
        try:
            cookies = json.loads(cf.read_text(encoding="utf-8"))
        except Exception:
            continue
        try:
            status, data = _dash_request(cookies, "GET", f"/zones/{zone_id}")
        except Exception:
            continue
        if isinstance(data, dict) and data.get("success"):
            return cf
    return None


def _ensure_email_routing_via_dashboard(output_root, inputs, worker_name: str) -> bool:
    """Configure Email Routing through the saved dash session: enable + catch-all->Worker.

    Scoped API tokens can be denied Email Routing settings (HTTP 10000) even when
    the Token carries Email Routing Rules Write, so configuration falls back to the
    Phase 1 dashboard session for accounts that expose Routing only to the dashboard.
    Enabling Routing on a Cloudflare zone auto-provisions the apex MX/SPF/DKIM/TXT
    records (route1..3.mx.cloudflare.net) required by wildcard-MX receipt verification.
    """
    cf = _find_dash_cookie_for_zone(output_root, inputs.zone_id)
    if cf is None:
        return False
    cookies = json.loads(cf.read_text(encoding="utf-8"))
    _dash_request(cookies, "POST", f"/zones/{inputs.zone_id}/email/routing/enable")
    status, data = _dash_request(
        cookies, "PUT", f"/zones/{inputs.zone_id}/email/routing/rules/catch_all",
        {"matchers": [{"type": "all"}],
         "actions": [{"type": "worker", "value": [worker_name]}],
         "enabled": True},
    )
    return isinstance(data, dict) and bool(data.get("success"))


def ensure_email_routing(client: Any, inputs: DeployInputs, worker_name: str, output_root=None) -> Mapping[str, Any]:
    try:
        return _ensure_email_routing_strict(client, inputs, worker_name)
    except DeploymentError as exc:
        if _permission_limited(exc):
            # Some Cloudflare accounts expose Email Routing only to the dashboard
            # session, never to scoped API tokens. When the token is rejected on
            # permission grounds, fall back to the Phase 1 dash session (enables
            # the zone and auto-provisions apex MX/SPF/DKIM) so real receipt can
            # still be proven downstream. Reuse is optional: without a saved
            # cookie we note the limitation and let real receipt be verified anyway.
            try:
                if output_root and _ensure_email_routing_via_dashboard(output_root, inputs, worker_name):
                    return {"configured_via": "dashboard-cookie", "dashboard_session": "live"}
            except Exception:
                pass
            print(
                "  Email Routing is managed outside the API token scope; "
                "skipping in-CLI routing configuration",
                flush=True,
            )
            return {"permission_limited": True}
        raise


def _ensure_email_routing_strict(client: Any, inputs: DeployInputs, worker_name: str) -> Mapping[str, Any]:
    """Ensure account/zone routing and an exact worker catch-all target."""
    account = _api_call(client, ("get_email_routing_account", "email_routing_account"), inputs.account_id)
    zone = _api_call(client, ("get_email_routing_zone", "email_routing_zone"), inputs.zone_id)
    for label, obj in (("account", account), ("zone", zone)):
        expected = inputs.account_id if label == "account" else inputs.zone_id
        observed = obj.get("id", obj.get(f"{label}_id")) if isinstance(obj, Mapping) else None
        if not isinstance(obj, Mapping) or observed != expected:
            raise DeploymentError(f"email routing {label} ownership is not confirmed")
        if obj.get("enabled") is False or obj.get("status") in ("unconfirmed", "disabled", "pending"):
            if label == "zone" and hasattr(client, "enable_email_routing"):
                print("  Enabling Cloudflare Email Routing", flush=True)
                _api_call(client, ("enable_email_routing",), inputs.zone_id)
                obj = _api_call(client, ("get_email_routing_zone", "email_routing_zone"), inputs.zone_id)
                if not obj.get("enabled") or obj.get("status") in ("unconfirmed", "disabled", "pending"):
                    raise DeploymentError("email routing zone remains unconfirmed after enable")
            else:
                raise DeploymentError(f"email routing {label} prerequisite is unconfirmed; recover with: verify {label} email routing")
    rules = list(_api_call(client, ("list_email_routing_rules", "list_routing_rules"), inputs.zone_id))
    def is_catchall(r: Mapping[str, Any]) -> bool:
        if str(r.get("name", "")).lower() == "catch-all" or r.get("match_kind") == "catch_all":
            return True
        return any(m.get("type") == "all" for m in (r.get("matchers") or []))
    owned = [r for r in rules if is_catchall(r)]
    if len(owned) > 1:
        raise DeploymentError("ambiguous catch-all email routing rule")
    if owned:
        rule = owned[0]
        target = rule.get("worker", rule.get("worker_name"))
        if target is None:
            target = next((a.get("value") for a in (rule.get("actions") or []) if a.get("type") == "worker"), None)
        if isinstance(target, list) and len(target) == 1:
            target = target[0]
        actions = rule.get("actions") or []
        workers = [a.get("value") for a in actions if a.get("type") == "worker"]
        workers = [w[0] if isinstance(w, list) and len(w) == 1 else w for w in workers]
        if len(actions) == 1 and actions[0].get("type") == "drop":
            rule = _api_call(client, ("update_email_routing_rule",), inputs.zone_id, rule.get("id") or rule.get("tag"), worker_name)
        elif len(actions) != 1 or len(workers) != 1 or workers[0] != worker_name or target != worker_name:
            raise DeploymentError("catch-all rule has a different Worker")
    else:
        rule = _api_call(client, ("create_email_routing_rule", "create_routing_rule"), inputs.zone_id, inputs.base_domain, worker_name)
        if not isinstance(rule, Mapping) or not is_catchall(rule):
            raise DeploymentError("created catch-all rule response is malformed")
        actions = rule.get("actions") or []
        value = actions[0].get("value") if actions else None
        if isinstance(value, list) and len(value) == 1:
            value = value[0]
        if len(actions) != 1 or actions[0].get("type") != "worker" or value != worker_name:
            raise DeploymentError("created catch-all rule targets a different Worker")
    return rule


_APEX_MX_TARGETS = (
    ("route1.mx.cloudflare.net", 0), ("route2.mx.cloudflare.net", 10),
    ("route3.mx.cloudflare.net", 20),
)


def ensure_apex_mx(client: Any, inputs: DeployInputs) -> None:
    """Create CF-standard apex MX records when a fresh zone has none yet.

    A brand-new Phase 1 zone usually has no email routing: without apex MX the
    wildcard-MX receipt payloads have nothing to fan out from. If the apex is bare
    we create route1..3.mx.cloudflare.net so both this deploy and downstream
    wildcard-mx verification work over a scoped token (DNS Write, not Email
    Routing settings which some accounts only expose to the dashboard).
    """
    apex = inputs.base_domain.strip().rstrip(".").lower()
    records = list(_api_call(client, ("list_dns_records", "list_mx_records"), inputs.zone_id, apex))
    has_mx = any(
        str(row.get("type", "")).upper() == "MX"
        and str(row.get("name", "")).strip().rstrip(".").lower() in (apex, "@")
        for row in records
    )
    if has_mx:
        return
    try:
        for target, priority in _APEX_MX_TARGETS:
            _api_call(client, ("create_dns_record", "create_wildcard_mx"), inputs.zone_id,
                      {"type": "MX", "name": apex, "content": target, "priority": priority, "ttl": 300})
    except Exception:
        # Some scoped tokens carry DNS read but not write; the dashboard fallback
        # earlier already had a chance to provision apex MX via Email Routing.
        # Do not fail the whole deploy just for an optional apex CREATE — let the
        # strict wildcard path re-raise if it truly cannot proceed.
        pass


def ensure_wildcard_mx(client: Any, inputs: DeployInputs) -> list[Mapping[str, Any]]:
    ensure_apex_mx(client, inputs)
    records = list(_api_call(client, ("list_dns_records", "list_mx_records"), inputs.zone_id, inputs.base_domain))
    payloads = wildcard_mx_payloads(records, inputs.base_domain)
    existing = list(_api_call(client, ("list_wildcard_mx", "list_dns_wildcard_mx"), inputs.zone_id, inputs.base_domain))
    expected_pairs = {(p["content"], p.get("priority")) for p in payloads}
    for row in existing:
        if str(row.get("name", "")).rstrip(".").lower() != f"*.{inputs.base_domain}":
            continue
        pair = (row.get("content", row.get("target")), row.get("priority"))
        if pair not in expected_pairs:
            if any(pair[0] == ep[0] for ep in expected_pairs):
                raise DeploymentError("conflicting wildcard MX priority exists")
            raise DeploymentError("unrelated wildcard MX record exists")
    for payload in payloads:
        same = [r for r in existing if str(r.get("name", "")).rstrip(".").lower() == payload["name"] and r.get("content", r.get("target")) == payload["content"] and r.get("priority") == payload.get("priority")]
        if not same:
            _api_call(client, ("create_dns_record", "create_wildcard_mx"), inputs.zone_id, payload)
    return payloads


@dataclass(frozen=True)
class DeployInputs:
    credential_id: str
    account_id: str
    zone_id: str
    base_domain: str
    api_token: str
    worker_subdomain: str = "work"
    pages_subdomain: str = "page"


@dataclass(frozen=True)
class ResourceNames:
    worker: str
    pages: str
    d1: str
    kv: str


class SubprocessRunner:
    def run(self, argv: Sequence[str], cwd: Path, env: Mapping[str, str] | None = None):
        merged = os.environ.copy()
        if env:
            merged.update(env)
        completed = subprocess.run(
            list(argv), cwd=cwd, env=merged, text=True, capture_output=True, check=False
        )
        return {
            "returncode": completed.returncode,
            "stdout": completed.stdout,
            "stderr": completed.stderr,
        }


def _now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _normalized_permission(value: Any) -> str:
    if isinstance(value, Mapping):
        value = value.get("name", "")
    return " ".join(str(value).lower().replace(":", " ").replace("_", " ").split())


def _is_deployable(record: Mapping[str, Any]) -> bool:
    token = record.get("api_token")
    if (
        not isinstance(token, str)
        or len(token.strip()) < 20
        or "..." in token
        or any(marker in token for marker in ("*", "•", "…"))
    ):
        return False
    permissions = record.get("token_permissions", record.get("permissions"))
    names = {_normalized_permission(item) for item in permissions} if isinstance(permissions, list) else set()
    if not all(group.intersection(names) for group in REQUIRED_CAPABILITIES):
        return False
    verification = record.get("token_verification", record.get("verification"))
    return isinstance(verification, Mapping) and all(
        isinstance(verification.get(name), Mapping)
        and verification[name].get("success") is True
        for name in PROOFS
    )


def _deploy_inputs(root: Path, record: Mapping[str, Any]) -> DeployInputs:
    binding_owner = str(record.get("source_credential_id") or record.get("credential_id") or "")
    try:
        # A Cloudflare account may hold several domains (phase1 --cf-credential/
        # --kata-credential registers additional domains under the same account,
        # appending one verified binding per domain). Pick the binding matching
        # this token's own zone_id/domain instead of always picking the newest,
        # so an older token stays deployable after a second domain is registered.
        binding = select_binding(
            root,
            binding_owner,
            zone_id=str(record.get("zone_id") or "") or None,
            domain=str(record.get("domain") or "") or None,
        )
    except (KeyError, LedgerError) as exc:
        raise DeploymentError(str(exc)) from exc
    if not _is_deployable(record):
        raise DeploymentError(
            f"credential {record.get('credential_id')!r} lacks a complete verified token"
        )
    domain = str(binding["domain"]).strip().rstrip(".").lower()
    if not domain or any(record.get(field) != binding.get(field) for field in ("account_id", "zone_id")):
        raise DeploymentError("credential account/zone/domain ownership is not exact")
    if str(record.get("domain", "")).strip().rstrip(".").lower() != domain:
        raise DeploymentError("credential domain ownership is not exact")
    return DeployInputs(
        credential_id=str(record["credential_id"]), account_id=str(binding["account_id"]),
        zone_id=str(binding["zone_id"]), base_domain=domain,
        api_token=str(record["api_token"]),
    )


def load_deploy_inputs(root: Path, explicit_id: str | None) -> DeployInputs:
    """Select an exact explicit credential or the latest fully deployable one."""
    root = Path(root)
    if explicit_id is not None:
        try:
            record = select_credential(root, "cloudflare", explicit_id)
        except LedgerError as exc:
            raise DeploymentError(str(exc)) from exc
        return _deploy_inputs(root, record)

    records = read_records(root, "cloudflare")
    ranked = []
    for index, record in enumerate(records):
        if record.get("platform") != "cloudflare" or record.get("status") not in ("active", "verified"):
            continue
        if record.get("phase") and record.get("phase") != "phase2":
            continue
        try:
            timestamp = datetime.fromisoformat(str(record["created_at"]).replace("Z", "+00:00"))
            inputs = _deploy_inputs(root, record)
        except (KeyError, ValueError, DeploymentError):
            continue
        ranked.append((timestamp, index, inputs))
    if not ranked:
        raise DeploymentError("no valid Cloudflare credential with a complete verified token and exact binding")
    return max(ranked, key=lambda item: (item[0], item[1]))[2]


def resource_names(inputs: DeployInputs) -> ResourceNames:
    domain = inputs.base_domain.strip().rstrip(".").lower()
    slug = re.sub(r"[^a-z0-9]+", "-", domain).strip("-")[:32] or "domain"
    material = "\0".join((inputs.credential_id, inputs.account_id, inputs.zone_id, domain))
    suffix = hashlib.sha256(material.encode()).hexdigest()[:10]
    stem = f"tempmail-{slug}-{suffix}"
    return ResourceNames(
        worker=f"tempmail-{slug}-{suffix}", pages=f"tempmail-ui-{slug}-{suffix}",
        d1=f"{stem}-db", kv=f"{stem}-kv",
    )


def _ensure_resource(
    kind: str, client: Any, inputs: DeployInputs, name: str,
    expected_id: str | None = None,
) -> Mapping[str, str]:
    rows = list(getattr(client, f"list_{kind}")(inputs.account_id))
    matches = [row for row in rows if row.get("name") == name]
    if len(matches) > 1:
        raise DeploymentError(f"ambiguous {kind} resource named {name!r}")
    if matches:
        row = matches[0]
        if not row.get("id") or row.get("account_id") != inputs.account_id:
            raise DeploymentError(f"unproven {kind} ownership for {name!r}")
        if expected_id is not None and row.get("id") != expected_id:
            raise DeploymentError(f"saved {kind} id does not match remote resource {name!r}")
        return row
    created = getattr(client, f"create_{kind}")(inputs.account_id, name)
    if created.get("name") != name or created.get("account_id") != inputs.account_id or not created.get("id"):
        raise DeploymentError(f"created {kind} identity or account mismatch")
    if expected_id is not None and created.get("id") != expected_id:
        raise DeploymentError(f"recreated {kind} id does not match saved ownership")
    return created


def ensure_d1(
    client: Any, inputs: DeployInputs, names: ResourceNames,
    expected_id: str | None = None,
) -> Mapping[str, str]:
    return _ensure_resource("d1", client, inputs, names.d1, expected_id)


def ensure_kv(
    client: Any, inputs: DeployInputs, names: ResourceNames,
    expected_id: str | None = None,
) -> Mapping[str, str]:
    return _ensure_resource("kv", client, inputs, names.kv, expected_id)


def ensure_pages(
    client: Any, inputs: DeployInputs, names: ResourceNames,
    expected_id: str | None = None,
) -> Mapping[str, str]:
    return _ensure_resource("pages", client, inputs, names.pages, expected_id)


def ensure_pages_custom_domain(
    client: Any, inputs: DeployInputs, project: str,
    health_sleep: Callable[[float], None] = time.sleep,
) -> str | None:
    """Attach a `page.<domain>` custom domain to the Pages project.

    Returns the custom-domain host (e.g. ``page.example.com``) once the Pages
    domain is provisioned and its proxied CNAME to ``<project>.pages.dev`` is in
    place, or None when the client cannot manage Pages domains / custom domains
    (the deployment then keeps the pages.dev URL).
    """
    sub = f"{inputs.pages_subdomain}.{inputs.base_domain}"
    target = f"{project}.pages.dev"
    if not hasattr(client, "create_pages_domain") or not hasattr(client, "create_dns_record"):
        return None
    try:
        _api_call(client, ("create_pages_domain",), inputs.account_id, project, sub)
    except Exception:
        pass  # already attached or token cannot manage Pages domains
    # Pages requires a CNAME to the project's pages.dev host ("CNAME record not set")
    attached = False
    if hasattr(client, "list_dns_by_name"):
        try:
            rows = list(_api_call(client, ("list_dns_by_name",), inputs.zone_id, sub))
            attached = any(str(r.get("type", "")).upper() == "CNAME" for r in rows)
        except Exception:
            pass
    if not attached:
        try:
            _api_call(client, ("create_dns_record",), inputs.zone_id, {
                "type": "CNAME", "name": sub, "content": target,
                "zone_id": inputs.zone_id, "proxied": True, "ttl": 1,
            })
            attached = True
        except Exception:
            return None
    # Wait for the Pages custom domain to flip to active (CF status can lag actual
    # reachability); a proxied CNAME is already enough to serve the site.
    try:
        rows = list(_api_call(client, ("list_pages_domains",), inputs.account_id, project))
    except Exception:
        rows = []
    for _ in range(15):
        row = next((r for r in rows if str(r.get("name", "")).strip().rstrip(".").lower() == sub), None)
        if row and str(row.get("status") or "").lower() == "active":
            return sub
        health_sleep(3.0)
        try:
            rows = list(_api_call(client, ("list_pages_domains",), inputs.account_id, project))
        except Exception:
            break
    return sub if attached else None


def _validate_copy_tree(path: Path, source_root: Path) -> None:
    for candidate in path.rglob("*"):
        if candidate.is_symlink():
            try:
                candidate.resolve(strict=False).relative_to(source_root.resolve())
            except ValueError as exc:
                raise DeploymentError(f"source symlink escapes /workspace: {candidate}") from exc
            raise DeploymentError(f"source symlink is not permitted: {candidate}")


def copy_build_sources(source_root: Path, output_root: Path, deployment_id: str) -> Path:
    source_root, output_root = Path(source_root), Path(output_root)
    if source_root.is_symlink():
        raise DeploymentError(f"source root is a symlink: {source_root}")
    if not re.fullmatch(r"[A-Za-z0-9._-]+", deployment_id):
        raise DeploymentError("invalid deployment_id path component")
    if output_root.is_symlink() or (output_root / "build").is_symlink():
        raise DeploymentError("writable output/build root must not be a symlink")
    build_parent = output_root / "build"
    build_root = build_parent / deployment_id
    try:
        build_root.resolve(strict=False).relative_to(build_parent.resolve(strict=False))
    except ValueError as exc:
        raise DeploymentError("build path escapes output root") from exc
    if build_root.exists():
        marker = build_root / ".resend-modular-build.json"
        try:
            ownership = json.loads(marker.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise DeploymentError(f"existing directory lacks a valid owned build marker: {build_root}") from exc
        if ownership.get("deployment_id") != deployment_id or ownership.get("output_root") != str(output_root.resolve()):
            raise DeploymentError(f"existing directory lacks the expected owned build marker: {build_root}")
        shutil.rmtree(build_root)
    build_root.mkdir(parents=True)
    marker = build_root / ".resend-modular-build.json"
    marker.write_text(
        json.dumps({"deployment_id": deployment_id, "output_root": str(output_root.resolve())}),
        encoding="utf-8",
    )
    marker.chmod(0o600)
    for name in SOURCE_DIRS:
        source = source_root / name
        if source.is_symlink():
            raise DeploymentError(f"required source directory is a symlink: {source}")
        if not source.is_dir():
            raise DeploymentError(f"required source directory missing: {name}")
        _validate_copy_tree(source, source_root)
        shutil.copytree(source, build_root / name, symlinks=False)
    for name in ROOT_METADATA:
        source = source_root / name
        if source.exists():
            if source.is_symlink():
                raise DeploymentError(f"source symlink is not permitted: {source}")
            shutil.copy2(source, build_root / name)
    return build_root


def _toml_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=False)


def render_wrangler(
    inputs: DeployInputs, names: ResourceNames, d1: Mapping[str, str], kv: Mapping[str, str],
    jwt_secret: str, admin_password: str,
) -> str:
    domain = _toml_string(inputs.base_domain)
    return f'''name = {_toml_string(names.worker)}
main = "src/worker.ts"
workers_dev = false
routes = [{{ pattern = "{inputs.worker_subdomain}.{inputs.base_domain}/*", zone_id = {_toml_string(inputs.zone_id)} }}]
compatibility_date = "2025-04-01"
compatibility_flags = ["nodejs_compat"]
send_email = [{{ name = "SEND_MAIL" }}]

[vars]
DOMAINS = [{domain}]
DEFAULT_DOMAINS = [{domain}]
RANDOM_SUBDOMAIN_DOMAINS = [{domain}]
RANDOM_SUBDOMAIN_LENGTH = 8
SEND_MAIL_DOMAINS = [{domain}]
JWT_SECRET = {_toml_string(jwt_secret)}
ADMIN_PASSWORDS = [{_toml_string(admin_password)}]

[[d1_databases]]
binding = "DB"
database_name = {_toml_string(str(d1.get("name", names.d1)))}
database_id = {_toml_string(str(d1["id"]))}

[[kv_namespaces]]
binding = "KV"
id = {_toml_string(str(kv["id"]))}

[ai]
binding = "AI"
'''


def _result(result: Any) -> tuple[int, str, str]:
    if isinstance(result, Mapping):
        return int(result.get("returncode", 0)), str(result.get("stdout", "")), str(result.get("stderr", ""))
    return int(result.returncode), str(result.stdout or ""), str(result.stderr or "")


def _safe_output(stdout: str, stderr: str, secrets_to_redact: Sequence[str] = ()) -> str:
    text = (stderr or stdout or "no diagnostic output").strip().replace("\n", " ")[:1000]
    for secret in secrets_to_redact:
        if secret:
            text = text.replace(secret, "[REDACTED]")
    return text


def run_checked(runner: Any, argv: Sequence[str], cwd: Path, env: Mapping[str, str] | None = None, secrets_to_redact: Sequence[str] = ()) -> str:
    code, stdout, stderr = _result(runner.run(argv, cwd=Path(cwd), env=env))
    if code:
        raise CommandError(code, _safe_output(stdout, stderr, secrets_to_redact))
    return stdout


def initialize_database(
    runner: Any, build_root: Path, d1: Mapping[str, str], wrangler_path: Path,
    env: Mapping[str, str] | None = None,
    completed: set[str] | None = None,
    on_applied: Callable[[str], None] | None = None,
) -> None:
    db_root = Path(build_root) / "db"
    schema = db_root / "schema.sql"
    migrations = ([schema] if schema.exists() else []) + sorted(
        path for path in db_root.glob("*.sql") if path.name != "schema.sql"
    )
    if not migrations:
        raise DeploymentError("no D1 schema or migrations found")
    worker_root = Path(build_root) / "worker"
    completed = completed or set()
    for migration in migrations:
        if migration.name in completed:
            continue
        print(f"  D1 migration: {migration.name}", flush=True)
        # The current schema already contains message_id on raw_mails; this
        # historical patch targets the removed legacy `mails` table.
        if migration.name == "2024-01-13-patch.sql" and schema.exists() and "create table mails" not in schema.read_text(encoding="utf-8").lower():
            if on_applied is not None:
                on_applied(migration.name)
            continue
        try:
            command = ("pnpm", "exec", "wrangler", "d1", "execute", str(d1["id"]),
                       "--remote", "--config", str(wrangler_path), "--file", str(migration))
            for attempt in range(3):
                try:
                    run_checked(runner, command, worker_root, env=env)
                    break
                except CommandError as exc:
                    if "fetch failed" not in exc.safe_output.lower() or attempt == 2:
                        raise
                    print(f"    Wrangler network retry {attempt + 1}/2", flush=True)
                    time.sleep(2)
        except CommandError as exc:
            # A prior remote execution may have committed the migration before
            # the local ledger write. Treat SQLite duplicate-column/table
            # errors as already-applied migrations so retries are safe.
            if not re.search(r"duplicate column name|already exists", exc.safe_output, re.I):
                raise
        if on_applied is not None:
            on_applied(migration.name)
        print(f"  D1 migration complete: {migration.name}", flush=True)


def _extract_url(output: str, label: str) -> str:
    matches = URL_RE.findall(output)
    if not matches:
        raise DeploymentError(f"unable to discover {label} URL")
    return matches[-1].rstrip(".,)")


def _default_http_get(url: str) -> tuple[int, str]:
    return _curl_http("GET", url, headers={"User-Agent": "resend-modular-deployer/1"}, timeout=20)


def _deployment_id(inputs: DeployInputs) -> str:
    material = "\0".join((inputs.credential_id, inputs.account_id, inputs.zone_id, inputs.base_domain))
    return "deploy-" + hashlib.sha256(material.encode()).hexdigest()[:20]


def _random_secret(length: int = 48) -> str:
    alphabet = string.ascii_letters + string.digits
    return "".join(secrets.choice(alphabet) for _ in range(length))


def _saved_secrets(root: Path, deployment_id: str, inputs: DeployInputs) -> tuple[str, str] | None:
    matches = [row for row in read_records(root, "deployments") if row.get("deployment_id") == deployment_id]
    for row in reversed(matches):
        ownership = (row.get("credential_id"), row.get("account_id"), row.get("zone_id"), row.get("domain"))
        expected = (inputs.credential_id, inputs.account_id, inputs.zone_id, inputs.base_domain)
        if ownership != expected:
            raise DeploymentError("saved deployment secrets cross an account or domain boundary")
        if row.get("jwt_secret") and row.get("admin_password"):
            return str(row["jwt_secret"]), str(row["admin_password"])
    return None


def _saved_deployment_state(root: Path, deployment_id: str, inputs: DeployInputs) -> dict[str, Any]:
    expected = (inputs.credential_id, inputs.account_id, inputs.zone_id, inputs.base_domain)
    state: dict[str, Any] = {"applied_migrations": []}
    migrations: list[str] = []
    for row in read_records(root, "deployments"):
        if row.get("deployment_id") != deployment_id:
            continue
        ownership = (row.get("credential_id"), row.get("account_id"), row.get("zone_id"), row.get("domain"))
        if ownership != expected:
            raise DeploymentError("saved deployment state crosses an account or domain boundary")
        for field in ("d1_id", "kv_id", "pages_id"):
            if row.get(field):
                if state.get(field) and state[field] != row[field]:
                    raise DeploymentError(f"conflicting saved deployment ownership for {field}")
                state[field] = row[field]
        for name in row.get("applied_migrations", []):
            if isinstance(name, str) and name not in migrations:
                migrations.append(name)
    state["applied_migrations"] = migrations
    return state


def _base_record(inputs: DeployInputs, names: ResourceNames, deployment_id: str, jwt: str, admin: str) -> dict[str, Any]:
    return {
        "deployment_id": deployment_id, "credential_id": inputs.credential_id,
        "account_id": inputs.account_id, "zone_id": inputs.zone_id,
        "domain": inputs.base_domain, "worker_name": names.worker,
        "pages_name": names.pages, "d1_name": names.d1, "kv_name": names.kv,
        "jwt_secret": jwt, "admin_password": admin,
    }


def build_mail_credentials(state: Mapping[str, Any], receive_identity: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """Build the final Phase-3 mail-service credential document for one domain.

    Pure builder: given the deployment state row (plus the optional receive
    identity / jwt), return the dict persisted as
    ``credentials/<domain>_mail_credentials.json``.  JSON has no comments, so
    usage instructions are carried inside the ``usage`` field as an array of
    copy-pasteable curl commands (per the project convention).
    """
    domain = str(state.get("domain") or "")
    worker_url = str(state.get("worker_url") or f"https://work.{domain}").rstrip("/")
    pages_url = state.get("pages_url") or ""
    admin_password = str(state.get("admin_password") or "")
    identity = dict(receive_identity or {})
    # 收件箱读取用的 JWT：优先回填本次接收验证身份，让 curl 可直接复制运行；
    # 没有身份时保留提示占位符 <ADDRESS_JWT>。
    address_jwt = str(identity.get("jwt") or "<ADDRESS_JWT>")
    usage = [
        "# 新建邮箱 / Create a mailbox（name 可自定义；enableRandomSubdomain=true 挂在随机子域名下）",
        (
            f'curl -skS -X POST "{worker_url}/admin/new_address" '
            f'-H "x-admin-auth: {admin_password}" -H "Content-Type: application/json" '
            f'-d \'{{"name":"myname","domain":"{domain}","enableRandomSubdomain":false}}\''
        ),
        "# 响应: {\"address\":\"myname@<domain>\",\"jwt\":\"<ADDRESS_JWT>\",\"address_id\":N}  —— 保存返回的 jwt，它就是该邮箱的读取凭证",
        "# 随机子域名版本（挂在 <random>." + domain + " 下，同样能收信）",
        (
            f'curl -skS -X POST "{worker_url}/admin/new_address" '
            f'-H "x-admin-auth: {admin_password}" -H "Content-Type: application/json" '
            f'-d \'{{"name":"myname","domain":"{domain}","enableRandomSubdomain":true}}\''
        ),
        "# 读收件箱（列表）/ List inbox —— 用上一步拿到的 ADDRESS_JWT（默认为接收验证身份的 jwt）",
        (
            f'curl -skS "{worker_url}/api/mails?limit=20&offset=0" '
            f'-H "Authorization: Bearer {address_jwt}"'
        ),
        "# 解析后的收件箱（subject/from/text/html 字段）",
        (
            f'curl -skS "{worker_url}/api/parsed_mails?limit=20&offset=0" '
            f'-H "Authorization: Bearer {address_jwt}"'
        ),
        "# 读取单封原始邮件 / Get one raw mail by id",
        (
            f'curl -skS "{worker_url}/api/mail/<MAIL_ID>" '
            f'-H "Authorization: Bearer {address_jwt}"'
        ),
        "# 管理员全站收件箱（可选 address 过滤）/ Admin inbox query",
        (
            f'curl -skS "{worker_url}/admin/mails?limit=20&offset=0&address=<mailbox>@<domain|sub>" '
            f'-H "x-admin-auth: {admin_password}"'
        ),
        "# 以管理员身份发信 / Admin send mail",
        (
            f'curl -skS -X POST "{worker_url}/admin/send_mail" '
            f'-H "x-admin-auth: {admin_password}" -H "Content-Type: application/json" '
            f'-d \'{{"from_name":"","from_mail":"<sender>@<domain>",'
            f'"to_mail":"someone@example.com","to_name":"",'
            f'"subject":"hi","content":"hello","is_html":false}}\''
        ),
        "# 注意：宿主 DNS 对 work.* 子域可能解析失败时用 IP 固定："
        " dig @1.1.1.1 +short work.<domain> | head -1 拿到 IP 后 "
        "给每条 curl 追加 --resolve work.<domain>:443:<IP>",
    ]
    doc: dict[str, Any] = {
        "kind": "tempmail-mail-service-credentials",
        "domain": domain,
        "deployment_id": state.get("deployment_id"),
        "credential_id": state.get("credential_id"),
        "worker_url": worker_url,
        "pages_url": pages_url,
        "web_ui": f"{pages_url or worker_url}  （浏览器打开，用 admin_password 登录 admin 面板）",
        "admin": {
            "header": "x-admin-auth",
            "password": admin_password,
        },
        "jwt_secret": state.get("jwt_secret") or "",
        "receive_identity": identity,
        "usage": usage,
        "notes": [
            "所有密钥按项目约定明文保存（plaintext by design），请勿泄露本文件。",
            "ADDRESS_JWT 是单个邮箱的读信凭证；admin password 可建/删任意邮箱、查全站邮件。",
            "本文件由 lib/deploy_tempmail.py 在 Phase 3 成功时自动生成/更新，可安全重新生成。",
        ],
    }
    return doc


def write_mail_credentials(credentials_root: Any, state: Mapping[str, Any], receive_identity: Mapping[str, Any] | None = None) -> Path:
    """Persist the Phase-3 mail-service credential file and return its path."""
    doc = build_mail_credentials(state, receive_identity)
    path = Path(credentials_root) / f"{doc['domain']}_mail_credentials.json"
    path.write_text(json.dumps(doc, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return path


def _append_state(root: Path, base: Mapping[str, Any], status: str, **updates: Any) -> dict[str, Any]:
    row = dict(base); row.update(updates); row["status"] = status; row["created_at"] = _now()
    return append_record(root, "deployments", row)


def ensure_worker_subdomain_dns(client: Any, inputs: DeployInputs) -> None:
    """Make the worker subdomain (e.g. ``work.<domain>``) resolve through Cloudflare.

    A Workers zone route does not auto-provision DNS for its subdomain. Without a
    proxied record the health check / API calls fail with NXDOMAIN. Creating a
    proxied A record (placeholder target) turns on the proxy so the route forwards
    the subdomain to the deployed Worker.
    """
    name = f"{inputs.worker_subdomain}.{inputs.base_domain}"
    if hasattr(client, "list_dns_by_name") and hasattr(client, "create_dns_record"):
        try:
            rows = list(_api_call(client, ("list_dns_by_name",), inputs.zone_id, name))
        except Exception:
            return
        if any(str(r.get("type", "")).upper() == "A" for r in rows):
            return
        try:
            _api_call(client, ("create_dns_record",), inputs.zone_id, {
                "type": "A", "name": name, "content": "192.0.2.1",
                "zone_id": inputs.zone_id, "proxied": True, "ttl": 1,
            })
        except Exception:
            pass


def deploy_core(
    credentials_root: Path, source_root: Path, output_root: Path, explicit_id: str | None,
    client: Any, runner: Any, http_get: Callable[[str], tuple[int, str]] = _default_http_get,
    receive_client: Any | None = None, receive_verify: str = "auto",
    health_sleep: Callable[[float], None] = time.sleep,
) -> dict[str, Any]:
    if receive_client is None:
        # Formal Phase 3 deployments must prove real receipt before succeeding.
        # Callers needing only resource preparation should use deploy_core_resources.
        raise DeploymentError("receive verification client is required for formal deployment")
    inputs = load_deploy_inputs(credentials_root, explicit_id)
    names = resource_names(inputs); deployment_id = _deployment_id(inputs)
    saved = _saved_secrets(credentials_root, deployment_id, inputs)
    saved_state = _saved_deployment_state(credentials_root, deployment_id, inputs)
    jwt_secret, admin_password = saved or (_random_secret(64), _random_secret(32))
    base = _base_record(inputs, names, deployment_id, jwt_secret, admin_password)
    _append_state(credentials_root, base, "selected")
    state = dict(base)
    routing_started = False
    try:
        build_root = copy_build_sources(source_root, output_root, deployment_id)
        state["build_root"] = str(build_root)
        _append_state(credentials_root, state, "preparing")
        for project in (build_root / "worker", build_root / "frontend"):
            run_checked(runner, ("pnpm", "install", "--frozen-lockfile"), project)
        d1 = ensure_d1(client, inputs, names, saved_state.get("d1_id"))
        kv = ensure_kv(client, inputs, names, saved_state.get("kv_id"))
        state.update(d1_id=d1["id"], kv_id=kv["id"])
        _append_state(credentials_root, state, "resources")
        wrangler = build_root / "worker" / "wrangler.generated.toml"
        wrangler.write_text(render_wrangler(inputs, names, d1, kv, jwt_secret, admin_password), encoding="utf-8")
        wrangler.chmod(0o600)
        cloudflare_env = {
            "CLOUDFLARE_API_TOKEN": inputs.api_token,
            "CLOUDFLARE_ACCOUNT_ID": inputs.account_id,
        }
        applied_migrations = list(saved_state.get("applied_migrations", []))

        def record_migration(name: str) -> None:
            if name not in applied_migrations:
                applied_migrations.append(name)
            state["applied_migrations"] = list(applied_migrations)
            _append_state(credentials_root, state, "database", wrangler_path=str(wrangler))

        initialize_database(
            runner, build_root, d1, wrangler, env=cloudflare_env,
            completed=set(applied_migrations), on_applied=record_migration,
        )
        state["applied_migrations"] = applied_migrations
        _append_state(credentials_root, state, "database", wrangler_path=str(wrangler))
        worker_output = run_checked(
            runner, ("pnpm", "exec", "wrangler", "deploy", "--config", str(wrangler)),
            build_root / "worker", env=cloudflare_env,
            secrets_to_redact=(inputs.api_token, jwt_secret, admin_password),
        )
        try:
            worker_url = _extract_url(worker_output, "Worker")
        except DeploymentError:
            # Zone-route deployments do not emit a workers.dev URL; the
            # configured route on the owned domain's worker subdomain is reachable.
            worker_url = f"https://{inputs.worker_subdomain}.{inputs.base_domain}"
        ensure_worker_subdomain_dns(client, inputs)
        # The freshly deployed Worker hostname can flap DNS/edge for a short window.
        # Treat a transport-level failure like a non-200 and let the ready-loop retry.
        status, body = None, ""
        healthy = False
        for attempt in range(31):
            if attempt:
                health_sleep(4.0)
            try:
                status, body = http_get(worker_url.rstrip("/") + "/health_check")
                if status == 200 and (body or "").strip() == "OK":
                    healthy = True
                    break
            except Exception:
                status, body = None, ""
        if not healthy:
            raise DeploymentError("Worker health check did not reach HTTP 200 OK")
        state["worker_url"] = worker_url
        _append_state(credentials_root, state, "worker")
        pages = ensure_pages(client, inputs, names, saved_state.get("pages_id"))
        state.update(pages_id=pages["id"], pages_name=pages["name"])
        run_checked(
            runner, ("pnpm", "run", "build"), build_root / "frontend",
            env={**cloudflare_env, "VITE_API_BASE": worker_url},
        )
        pages_output = run_checked(
            runner,
            ("pnpm", "exec", "wrangler", "pages", "deploy", "dist", "--project-name", names.pages, "--branch", "production"),
            build_root / "frontend", env=cloudflare_env,
            secrets_to_redact=(inputs.api_token, jwt_secret, admin_password),
        )
        pages_url = _extract_url(pages_output, "Pages")
        state["pages_url"] = pages_url
        custom = ensure_pages_custom_domain(client, inputs, names.pages, health_sleep=health_sleep)
        if custom:
            pages_url = f"https://{custom}"
            state["pages_url"] = pages_url
            state["pages_custom_domain"] = custom
        _append_state(credentials_root, state, "pages")
        # Routing is an explicit stage when the injected client supports it.
        # Older callers may provide a core-only fake client; preserve that contract.
        routing_started = True
        _append_state(credentials_root, state, "routing_pending")
        routing = ensure_email_routing(client, inputs, names.worker, output_root)
        wildcard = ensure_wildcard_mx(client, inputs)
        state["email_routing"] = dict(routing)
        state["wildcard_mx"] = list(wildcard)
        _append_state(credentials_root, state, "routing_configured")
        if receive_verify == "manual":
            worker_url = str(state.get("worker_url") or "").strip()
            if worker_url and hasattr(receive_client, "set_base_url"):
                receive_client.set_base_url(worker_url)
            base = receive_client.new_address(admin_password, False)
            random = receive_client.new_address(admin_password, True)
            recipient = str(random.get("address") or "")
            sender = str(base.get("address") or base.get("email") or "")
            jwt = str(random.get("jwt") or "")
            if recipient and jwt:
                identity = {"recipient": recipient, "sender": sender, "jwt": jwt}
                ident_path = Path(credentials_root) / f"{inputs.base_domain}_receive_identity.json"
                ident_path.write_text(json.dumps(identity, ensure_ascii=False, indent=2), encoding="utf-8")
                print(f"  [manual] receive identity saved to {ident_path}", flush=True)
            else:
                identity = None
            cred_path = write_mail_credentials(credentials_root, state, identity)
            print(f"  [mail-credentials] final mail-service credentials saved to {cred_path}", flush=True)
            state["receive_verification"] = {
                "status": "manual_pending",
                "recipient": recipient,
                "sender": sender,
                "note": "send any email to the random-subdomain address to verify receipt",
            }
            print(f"Manual receive verification: send any email to {recipient}", flush=True)
            return _append_state(credentials_root, state, "deployed_manual")
        smtp_config = _load_smtp_config(credentials_root)
        smtp_sender = _make_smtp_sender(smtp_config) if smtp_config else None
        prepare_send = None
        if smtp_sender:
            state["receive_verification_mode"] = "external_smtp"
        else:
            state["receive_verification_mode"] = "worker_send_mail"
            if getattr(client, "create_email_routing_destination_address", None) is not None:
                # New-account SEND_MAIL binding only delivers to destination
                # addresses verified at account level; run that loop first.
                def prepare_send(sender: str, recipient: str, recipient_jwt: str) -> Any:
                    return ensure_send_destination_verified(
                        client, account_id=inputs.account_id,
                        verify_address=recipient, verify_jwt=recipient_jwt,
                        worker=receive_client
                    )
        verification = verify_receive(receive_client, {**state, "admin_password": admin_password}, credentials_root, smtp_sender=smtp_sender, prepare_send=prepare_send)
        state["receive_verification"] = verification
        if verification.get("status") != "verified":
            raise DeploymentError(f"receive verification failed: {verification.get('reason', verification.get('status'))}")
        verified_identity = {
            "recipient": verification.get("recipient") or "",
            "sender": verification.get("sender") or "",
            "jwt": verification.get("jwt") or "",
        }
        if all(verified_identity.values()):
            ident_path = Path(credentials_root) / f"{inputs.base_domain}_receive_identity.json"
            ident_path.write_text(json.dumps(verified_identity, ensure_ascii=False, indent=2), encoding="utf-8")
        cred_path = write_mail_credentials(
            credentials_root, state,
            verified_identity if all(verified_identity.values()) else None,
        )
        print(f"  [mail-credentials] final mail-service credentials saved to {cred_path}", flush=True)
        return _append_state(credentials_root, state, "verified")
    except Exception as exc:
        reason = str(exc)
        for value in (inputs.api_token, jwt_secret, admin_password):
            reason = reason.replace(value, "[REDACTED]")
        _append_state(
            credentials_root, state, "routing_failed" if routing_started else "failed", failure_reason=reason[:1000],
            recovery="Remote resources were preserved; rerun with the same credential to continue safely.",
        )
        raise


class CloudflareApiClient:
    """Minimal token-authenticated Cloudflare REST adapter for the CLI.

    The orchestration layer deliberately depends on a narrow method surface so
    tests can inject fakes.  This adapter keeps the production entry point
    equally small and uses the complete plaintext token selected from the
    credential ledger.
    """

    def __init__(self, token: str, *, base_url: str = "https://api.cloudflare.com/client/v4", timeout: float = 30):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _request(self, method: str, path: str, payload: Mapping[str, Any] | None = None) -> Any:
        # Cloudflare 会按 TLS 指纹拦截 urllib.request（1010/URLError）；必须用 curl 子进程。
        import subprocess
        raw_io = json.dumps(payload) if payload is not None else None
        cmd = [
            "curl", "-sS", "-w", "\n%{http_code}", "-X", method,
            self.base_url + path,
            "-H", f"Authorization: Bearer {self.token}",
            "-H", "Content-Type: application/json",
            "--max-time", str(self.timeout),
        ]
        if raw_io is not None:
            cmd += ["--data-raw", raw_io]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        text = proc.stdout or ""
        separator = text.rfind("\n")
        if separator == -1 or not text[separator + 1:].strip().isdigit():
            detail = (text + proc.stderr).strip()[:300]
            raise DeploymentError(f"Cloudflare API request failed ({'CurlError'}): {detail}")
        status = int(text[separator + 1:].strip())
        raw = text[:separator]
        if status >= 400:
            detail = ""
            try:
                err_obj = json.loads(raw)
                err0 = ((err_obj.get("errors") or [{}]) or [{}])[0]
                if err0:
                    c = err0.get("code")
                    m = err0.get("message", "")
                    if c:
                        detail = f" (\"code\":{c} \"message\":\"{m[:120]}\")"
                    elif m:
                        detail = f" {m[:120]}"
            except Exception:
                pass
            raise DeploymentError(f"Cloudflare API HTTP {status}{detail}")
        try:
            data = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise DeploymentError("Cloudflare API returned invalid JSON") from exc
        if not isinstance(data, Mapping) or data.get("success") is not True:
            raise DeploymentError("Cloudflare API rejected request")
        return data.get("result")

    def list_d1(self, account_id: str) -> Sequence[Mapping[str, Any]]:
        rows = self._request("GET", f"/accounts/{account_id}/d1/database") or []
        return [dict(row, id=row.get("id") or row.get("uuid"), account_id=account_id) for row in rows]

    def create_d1(self, account_id: str, name: str) -> Mapping[str, Any]:
        row = self._request("POST", f"/accounts/{account_id}/d1/database", {"name": name})
        return dict(row, id=row.get("id") or row.get("uuid"), account_id=account_id)

    def list_kv(self, account_id: str) -> Sequence[Mapping[str, Any]]:
        rows = self._request("GET", f"/accounts/{account_id}/storage/kv/namespaces") or []
        return [dict(row, name=row.get("name") or row.get("title"), account_id=account_id) for row in rows]

    def create_kv(self, account_id: str, name: str) -> Mapping[str, Any]:
        row = self._request("POST", f"/accounts/{account_id}/storage/kv/namespaces", {"title": name})
        return dict(row, name=row.get("name") or row.get("title") or name, account_id=account_id)

    def list_pages(self, account_id: str) -> Sequence[Mapping[str, Any]]:
        rows = self._request("GET", f"/accounts/{account_id}/pages/projects") or []
        return [dict(row, account_id=account_id) for row in rows]

    def create_pages(self, account_id: str, name: str) -> Mapping[str, Any]:
        row = self._request("POST", f"/accounts/{account_id}/pages/projects", {"name": name, "production_branch": "production"})
        return dict(row, account_id=account_id)

    def get_pages(self, account_id: str, name: str) -> Mapping[str, Any]:
        return self._request("GET", f"/accounts/{account_id}/pages/projects/{name}") or {}

    def create_pages_domain(self, account_id: str, project: str, domain: str) -> Mapping[str, Any]:
        # Attaching a custom domain to a Pages project auto-provisions a proxied
        # DNS CNAME to the project's pages.dev host when the zone is in scope.
        return self._request("POST", f"/accounts/{account_id}/pages/projects/{project}/domains", {"name": domain}) or {}

    def list_pages_domains(self, account_id: str, project: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/accounts/{account_id}/pages/projects/{project}/domains") or []

    def get_email_routing_account(self, account_id: str) -> Mapping[str, Any]:
        # Cloudflare exposes routing configuration at the zone level; there is
        # no valid account-level /email/routing endpoint. Account ownership is
        # already proven by the selected credential and zone response.
        return {"id": account_id, "enabled": True}

    def get_email_routing_zone(self, zone_id: str) -> Mapping[str, Any]:
        return self._request("GET", f"/zones/{zone_id}/email/routing") or {}

    def enable_email_routing(self, zone_id: str) -> Mapping[str, Any]:
        return self._request("POST", f"/zones/{zone_id}/email/routing/enable") or {}

    def list_email_routing_rules(self, zone_id: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/zones/{zone_id}/email/routing/rules") or []

    def create_email_routing_rule(self, zone_id: str, domain: str, worker_name: str) -> Mapping[str, Any]:
        return self._request("POST", f"/zones/{zone_id}/email/routing/rules", {
            "name": "catch-all", "enabled": True,
            "matchers": [{"type": "all"}],
            "actions": [{"type": "worker", "value": [worker_name]}],
        })

    def list_email_routing_destination_addresses(self, account_id: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/accounts/{account_id}/email/routing/addresses") or []

    def create_email_routing_destination_address(self, account_id: str, email: str) -> Mapping[str, Any]:
        # 204 = address freshly created and a verification mail was emailed out; 409/2025
        # = the address already exists (already pending or verified) and must not be
        # treated as an error since a verified entry is the steady-state idempotence goal.
        try:
            return self._request("POST", f"/accounts/{account_id}/email/routing/addresses", {"email": email}) or {}
        except DeploymentError as exc:
            text = str(exc)
            if "409" in text or "2025" in text or "already exists" in text.lower():
                return {}
            raise

    def update_email_routing_rule(self, zone_id: str, rule_id: str, worker_name: str) -> Mapping[str, Any]:
        # Catch-all rules live behind a dedicated endpoint; PUT on
        # /rules/{rule_id} (which also rejects a synthesized tag) is answered
        # with HTTP 409 / code 2020 "Invalid rule operation" by Cloudflare.
        return self._request("PUT", f"/zones/{zone_id}/email/routing/rules/catch_all", {
            "name": "Catch-All", "enabled": True,
            "matchers": [{"type": "all"}],
            "actions": [{"type": "worker", "value": [worker_name]}],
        })

    def list_dns_records(self, zone_id: str, domain: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/zones/{zone_id}/dns_records?type=MX&name={domain}") or []

    def list_dns_by_name(self, zone_id: str, name: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/zones/{zone_id}/dns_records?name={name}") or []

    def list_wildcard_mx(self, zone_id: str, domain: str) -> Sequence[Mapping[str, Any]]:
        return self._request("GET", f"/zones/{zone_id}/dns_records?type=MX&name=*.{domain}") or []

    def create_dns_record(self, zone_id: str, payload: Mapping[str, Any]) -> Mapping[str, Any]:
        return self._request("POST", f"/zones/{zone_id}/dns_records", payload)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Deploy TempEmail Worker, Pages, routing, and verify receipt")
    parser.add_argument("--credentials-root", default="/app/credentials")
    parser.add_argument("--source-root", default="/workspace")
    parser.add_argument("--output-root", default="/app/output")
    parser.add_argument("--cf-credential", help="exact Cloudflare credential ID")
    parser.add_argument("--receive-verify", choices=("auto", "manual"), default="auto",
                        help="auto: prove receipt via SEND_MAIL/SMTP; manual: print a random-subdomain inbox for operator verification")
    args = parser.parse_args(argv)
    try:
        inputs = load_deploy_inputs(Path(args.credentials_root), args.cf_credential)
        client = CloudflareApiClient(inputs.api_token, base_url=os.environ.get("CLOUDFLARE_API_BASE_URL", "https://api.cloudflare.com/client/v4"))
        worker_url = os.environ.get("TEMPMAIL_WORKER_URL", "https://127.0.0.1")
        receive_client = WorkerHttpClient(worker_url, domain=inputs.base_domain)
        result = deploy_core(
            Path(args.credentials_root), Path(args.source_root), Path(args.output_root),
            args.cf_credential, client, SubprocessRunner(), receive_client=receive_client,
            receive_verify=args.receive_verify,
        )
        print(f"TempEmail deployment {args.receive_verify} verified: {result.get('deployment_id', '')}", flush=True)
        return 0
    except KeyboardInterrupt:
        print("TempEmail deployment interrupted", file=sys.stderr, flush=True)
        return 130
    except Exception as exc:
        # Never print exception text: it may contain a token, password, or JWT.
        import re as _re
        import traceback as _tb
        _detail = _re.sub(r"[A-Za-z0-9_\-\.]{60,}", "<TOKEN?>", _tb.format_exc())
        print(f"DEPLOY_DETAIL: {_detail[:2200]}", file=sys.stderr, flush=True)
        print(f"TempEmail deployment failed ({type(exc).__name__})", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
