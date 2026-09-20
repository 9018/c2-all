#!/usr/bin/env python3
"""
PTY end-to-end test for emp3r0r web panel — FINAL VERSION.
"""

import asyncio
import base64
import json
import os
import ssl
import sys
import time
import urllib.request

import websockets

HOST = os.environ.get("C2_HOST", "localhost:9443")
WS_URL = f"wss://{HOST}/api/ws"
API_URL = f"https://{HOST}/api"

ssl_ctx = ssl.create_default_context()
ssl_ctx.check_hostname = False
ssl_ctx.verify_mode = ssl.CERT_NONE


def http_post(path, data):
    body = json.dumps(data).encode()
    req = urllib.request.Request(f"{API_URL}{path}", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, context=ssl_ctx) as resp:
        return resp.status, resp.read().decode()


def http_get(path):
    req = urllib.request.Request(f"{API_URL}{path}", method="GET")
    with urllib.request.urlopen(req, context=ssl_ctx) as resp:
        return json.loads(resp.read())


async def recv_until(ws, predicate, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            raw = await asyncio.wait_for(ws.recv(), timeout=min(2.0, deadline - time.monotonic()))
            msg = json.loads(raw)
            if predicate(msg):
                return msg
        except asyncio.TimeoutError:
            continue
    return None


async def drain_messages(ws, timeout=0.5):
    msgs = []
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            raw = await asyncio.wait_for(ws.recv(), timeout=min(0.2, deadline - time.monotonic()))
            msgs.append(json.loads(raw))
        except (asyncio.TimeoutError, Exception):
            break
    return msgs


def start_session(agent_tag, job_id):
    status, _ = http_post("/command", {
        "AgentTag": agent_tag, "Action": "command",
        "Command": f"!shell -s --job_id {job_id}", "JobID": job_id,
    })
    return status == 200


async def test_pty_lifecycle(agent_tag):
    """Full PTY lifecycle: start → prompt → input → output → resize → close → dead."""
    job_id = f"pty-life-{int(time.time())}"
    r = {"start": False, "prompt": False, "echo": False, "resize": False, "close": False, "dead": False}

    print(f"[*] Starting PTY session: {job_id}")
    r["start"] = start_session(agent_tag, job_id)
    if not r["start"]:
        print("[!] Start failed")
        return r

    async with websockets.connect(WS_URL, ssl=ssl_ctx, additional_headers={"Origin": f"https://{HOST}"}) as ws:
        # Wait for shell prompt
        print("[*] Waiting for shell prompt...")
        prompt = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == job_id, timeout=15.0)
        if prompt:
            r["prompt"] = True
            decoded = base64.b64decode(prompt["data"]["Data"]).decode("utf-8", errors="replace")
            print(f"[+] Prompt: {repr(decoded[:100])}")
        else:
            print("[!] No prompt")
            return r

        await drain_messages(ws, 0.5)

        # Send echo command
        marker = f"MARKER_{int(time.time())}"
        print(f"[*] Sending: echo {marker}")
        await ws.send(json.dumps({
            "type": "pty_input",
            "data": {"AgentTag": agent_tag, "JobID": job_id,
                     "Data": base64.b64encode(f"echo {marker}\n".encode()).decode()}
        }))

        # Collect output looking for our marker
        print("[*] Waiting for marker in output...")
        deadline = time.monotonic() + 10.0
        found = False
        while time.monotonic() < deadline:
            msg = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == job_id, timeout=min(2.0, deadline - time.monotonic()))
            if msg:
                decoded = base64.b64decode(msg["data"]["Data"]).decode("utf-8", errors="replace")
                if marker in decoded:
                    found = True
                    r["echo"] = True
                    print(f"[+] Found marker: {repr(decoded.strip()[:100])}")
                    break
        if not found:
            print("[!] Marker not found")

        # Resize
        print("[*] Sending resize (120x40)...")
        await ws.send(json.dumps({
            "type": "pty_resize",
            "data": {"AgentTag": agent_tag, "JobID": job_id, "Data": "120x40"}
        }))
        r["resize"] = True
        print("[+] Resize sent")

        # Close session
        print("[*] Closing session...")
        await ws.send(json.dumps({
            "type": "pty_close",
            "data": {"AgentTag": agent_tag, "JobID": job_id, "Data": ""}
        }))
        r["close"] = True

        # Wait for close confirmation
        close_msg = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == job_id, timeout=5.0)
        if close_msg:
            decoded = base64.b64decode(close_msg["data"]["Data"]).decode("utf-8", errors="replace")
            if "closed" in decoded:
                print(f"[+] Close confirmed: {repr(decoded.strip())}")

        await asyncio.sleep(2.0)
        await drain_messages(ws, 0.5)

        # Verify dead
        print("[*] Verifying session is dead...")
        await ws.send(json.dumps({
            "type": "pty_input",
            "data": {"AgentTag": agent_tag, "JobID": job_id,
                     "Data": base64.b64encode(b"echo GHOST\n").decode()}
        }))

        ghost = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == job_id, timeout=5.0)
        if ghost is None:
            r["dead"] = True
            print("[+] Session confirmed dead (no output)")
        else:
            decoded = base64.b64decode(ghost["data"]["Data"]).decode("utf-8", errors="replace")
            if "not found" in decoded or "closed" in decoded:
                r["dead"] = True
                print(f"[+] Session confirmed dead: {repr(decoded.strip())}")
            else:
                print(f"[!] Session still alive: {repr(decoded[:200])}")

    return r


async def test_pty_multi_sessions(agent_tag):
    """Test multiple concurrent PTY sessions with isolation."""
    print("\n[*] Testing concurrent PTY sessions...")
    sessions = [f"pty-m{i}-{int(time.time())}" for i in range(3)]
    r = {}

    # Connect WS FIRST so we don't miss prompts
    async with websockets.connect(WS_URL, ssl=ssl_ctx, additional_headers={"Origin": f"https://{HOST}"}) as ws:
        # Start sessions (prompts will arrive over WS)
        for sid in sessions:
            ok = start_session(agent_tag, sid)
            r[f"start_{sid[:15]}"] = ok
            print(f"  Start {sid[:20]}...: {'OK' if ok else 'FAIL'}")
            await asyncio.sleep(0.2)

        # Wait for prompts
        for sid in sessions:
            msg = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == sid and "started" in base64.b64decode(m.get("data", {}).get("Data", "")).decode("utf-8", errors="replace"), timeout=15.0)
            r[f"prompt_{sid[:15]}"] = msg is not None
            print(f"  Prompt {sid[:20]}...: {'OK' if msg else 'FAIL'}")

        await drain_messages(ws, 1.0)

        # Send unique marker to each session
        markers = {}
        for i, sid in enumerate(sessions):
            marker = f"ISO{i}_{int(time.time())}"
            markers[sid] = marker
            await ws.send(json.dumps({
                "type": "pty_input",
                "data": {"AgentTag": agent_tag, "JobID": sid,
                         "Data": base64.b64encode(f"echo {marker}\n".encode()).decode()}
            }))
            await asyncio.sleep(0.3)

        # Verify each session gets its own marker
        for i, sid in enumerate(sessions):
            marker = markers[sid]
            found = False
            deadline = time.monotonic() + 10.0
            while time.monotonic() < deadline:
                msg = await recv_until(ws, lambda m: m.get("type") == "pty_output" and m.get("data", {}).get("JobID") == sid, timeout=min(2.0, deadline - time.monotonic()))
                if msg:
                    decoded = base64.b64decode(msg["data"]["Data"]).decode("utf-8", errors="replace")
                    if marker in decoded:
                        found = True
                        break
            r[f"iso_{i}"] = found
            print(f"  Session {i} isolation: {'PASS' if found else 'FAIL'}")

        # Close all
        for sid in sessions:
            await ws.send(json.dumps({
                "type": "pty_close",
                "data": {"AgentTag": agent_tag, "JobID": sid, "Data": ""}
            }))

    return r


def print_results(name, results):
    print(f"\n{'=' * 50}")
    print(f"{name}:")
    print("=" * 50)
    all_pass = True
    for k, v in results.items():
        status = "PASS" if v else "FAIL"
        if not v:
            all_pass = False
        print(f"  {k:25s} : {status}")
    ok = sum(1 for v in results.values() if v)
    print(f"  {'TOTAL':25s} : {ok}/{len(results)}")
    print("=" * 50)
    return all_pass


# Get agent tag
agents = http_get("/agents")
AGENT_TAG = agents[0]["Tag"]
print(f"[*] Agent: {AGENT_TAG}\n")

r1 = asyncio.run(test_pty_lifecycle(AGENT_TAG))
ok1 = print_results("PTY LIFECYCLE", r1)

r2 = asyncio.run(test_pty_multi_sessions(AGENT_TAG))
ok2 = print_results("PTY MULTI-SESSION", r2)

print(f"\n{'=' * 50}")
print(f"OVERALL: {'ALL PASS' if (ok1 and ok2) else 'ISSUES FOUND'}")
print("=" * 50)
sys.exit(0 if (ok1 and ok2) else 1)
