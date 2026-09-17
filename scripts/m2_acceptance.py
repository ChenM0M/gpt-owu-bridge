#!/usr/bin/env python3
"""Black-box M2 CLI acceptance with an isolated fictional OWU HTTP server.

Usage: python3 scripts/m2_acceptance.py /tmp/gpt-owu-gate
No real credentials, network target, or user conversations are used.
"""
import copy
import http.server
import json
import os
from pathlib import Path
import subprocess
import sqlite3
import sys
import tempfile
import threading

BINARY = str(Path(sys.argv[1]).resolve())
ROOT = Path(__file__).resolve().parents[1]
WORK = Path(tempfile.mkdtemp(prefix="gpt-owu-m2-acceptance-"))
STATE = {"chats": {}, "creates": 0, "updates": 0, "requests": [], "mode": "normal", "account": "mock-owner"}
TOKEN = "m2-synthetic-canary-secret"
CREATE_STARTED = threading.Event()
CREATE_RELEASE = threading.Event()


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send_json(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        STATE["requests"].append(("GET", self.path))
        if self.path == "/api/version":
            return self.send_json(200, {"version": "0.11.3", "deployment_id": "mock-deployment"})
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            return self.send_json(401, {"detail": "unauthorized"})
        if self.path == "/api/v1/auths/":
            return self.send_json(200, {"id": STATE["account"], "role": "user", "email": "test@example.invalid", "name": "Synthetic"})
        if self.path.startswith("/api/v1/chats/"):
            key = self.path.rsplit("/", 1)[-1]
            if STATE["mode"] == "read-fail":
                return self.send_json(503, {"detail": "synthetic read failure"})
            if key in STATE["chats"]:
                if STATE["mode"] == "graph-readback":
                    chat = STATE["chats"][key]["chat"]
                    ids = [m["id"] for m in chat["messages"]]
                    graph = chat["history"]["messages"]
                    graph[ids[0]]["childrenIds"] = [ids[1], ids[2]]
                    graph[ids[1]]["childrenIds"] = []
                    graph[ids[2]]["parentId"] = ids[0]
                    chat["messages"] = [copy.deepcopy(graph[i]) for i in [ids[0], *ids[2:]]]
                    STATE["mode"] = "normal"
                return self.send_json(200, STATE["chats"][key])
        return self.send_json(404, {})

    def do_POST(self):
        STATE["requests"].append(("POST", self.path))
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            return self.send_json(401, {})
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path == "/api/v1/chats/new":
            STATE["creates"] += 1
            key = f"00000000-0000-4000-8000-{STATE['creates']:012d}"
            row = {"id": key, "user_id": STATE["account"], "title": body["chat"]["title"],
                   "chat": copy.deepcopy(body["chat"]), "archived": False, "pinned": False,
                   "folder_id": None, "meta": {}, "created_at": 1700000000, "updated_at": 1700000000}
            row["chat"]["id"] = key
            STATE["chats"][key] = row
            if STATE["mode"] == "block-create":
                CREATE_STARTED.set()
                CREATE_RELEASE.wait(10)
                self.close_connection = True
                return
            if STATE["mode"] == "lost-receipt":
                self.close_connection = True
                return
            return self.send_json(200, row)
        if self.path.startswith("/api/v1/chats/"):
            key = self.path.rsplit("/", 1)[-1]
            if key in STATE["chats"]:
                STATE["updates"] += 1
                STATE["chats"][key]["chat"].update(body["chat"])
                STATE["chats"][key]["title"] = body["chat"]["title"]
                return self.send_json(200, STATE["chats"][key])
        return self.send_json(404, {})


SERVER = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=SERVER.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{SERVER.server_port}"
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GATE_")}
ENV.update(GATE_OWU_TOKEN=TOKEN, GATE_OWU_BASE_URL=BASE)
RESULTS = []


def run(directory, *args, ok=True):
    result = subprocess.run([BINARY, "sync", *args, "--", "--data-dir", str(directory)],
                            env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in result.stdout + result.stderr, "credential leak"
    if ok:
        assert result.returncode == 0, (args, result.stdout, result.stderr)
        return json.loads(result.stdout)
    assert result.returncode != 0, (args, result.stdout)
    return result


def check(name, condition):
    assert condition, name
    RESULTS.append(name)


def main():
    # Every command is a fresh process, including replay and recovery.
    directory = WORK / "normal"
    initialized = run(directory, "init")
    run(directory, "init", ok=False)
    check("explicit initialization cannot overwrite existing state", True)
    plan = run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"))["plan"]
    run(directory, "apply", "--plan", plan["id"], ok=False)
    check("apply requires confirmation", STATE["creates"] == 0)
    applied = run(directory, "apply", "--plan", plan["id"], "--confirm")["operation"]
    check("create plus semantic readback succeeds", applied["status"] == "succeeded" and STATE["creates"] == 1)
    binding = applied["binding_id"]
    target = next(iter(STATE["chats"]))
    check("target contains all five synthetic messages", len(STATE["chats"][target]["chat"]["messages"]) == 5)
    status = run(directory, "status", "--operation", applied["id"])["operation"]
    check("successful operation survives process restart", status == applied)
    replay = run(directory, "apply", "--plan", plan["id"], "--confirm")["operation"]
    check("same plan replay cannot create duplicate", replay["id"] == applied["id"] and STATE["creates"] == 1)
    repeat = run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"), "--binding", binding)["plan"]
    assert repeat["status"] == "no_change", repeat
    check("unchanged source and target becomes no change", True)
    candidate_change = WORK / "candidate-change.html"
    candidate_change.write_text((ROOT / "testdata/synthetic-share.html").read_text().replace("M1 合成分享：格式核对", "Candidate source title changed"))
    blocked = run(directory, "preview", "--html", str(candidate_change), "--binding", binding)["plan"]
    assert blocked["status"] == "conflict", blocked
    run(directory, "apply", "--plan", blocked["id"], "--confirm", ok=False)
    check("real CLI cannot assert stable candidate source identity", STATE["updates"] == 0)
    before = len(STATE["requests"])
    run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"), "--binding", "arbitrary-private-chat", ok=False)
    check("untrusted binding cannot read an arbitrary target", not any(path.endswith("arbitrary-private-chat") for _, path in STATE["requests"][before:]))
    STATE["account"] = "different-owner"
    before = len(STATE["requests"])
    run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"), "--binding", binding, ok=False)
    check("account switch blocks target access", not any(path.startswith("/api/v1/chats/") for _, path in STATE["requests"][before:]))
    STATE["account"] = "mock-owner"
    STATE["chats"][target]["chat"]["native_custom_field"] = {"private": "must survive"}
    conflicted = run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"), "--binding", binding)["plan"]
    check("native target mutation blocks unchanged-source sync", conflicted["status"] == "conflict")
    check("native content preserved without target writes", STATE["updates"] == 0 and "native_custom_field" in STATE["chats"][target]["chat"])
    run(directory, "apply", "--plan", repeat["id"], "--confirm", ok=False)
    check("stale no-change plan rechecks target before success", STATE["updates"] == 0)
    with sqlite3.connect(directory / "gate.db") as database:
        database.execute("UPDATE operations SET target_chat_id = ? WHERE operation_id = ?", ("inconsistent-creation-target", applied["id"]))
    before = len(STATE["requests"])
    run(directory, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"), "--binding", binding, ok=False)
    check("inconsistent creation evidence blocks even target GET", not any(path.startswith("/api/v1/chats/") for _, path in STATE["requests"][before:]))

    # A persisted receipt permits targeted read-only recovery after restart.
    receipt_dir = WORK / "receipt"
    run(receipt_dir, "init")
    receipt_plan = run(receipt_dir, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"))["plan"]
    STATE["mode"] = "read-fail"
    failed = subprocess.run([BINARY, "sync", "apply", "--plan", receipt_plan["id"], "--confirm", "--", "--data-dir", str(receipt_dir)], env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in failed.stdout + failed.stderr
    pending = json.loads(failed.stdout)["operation"]
    check("failed readback preserves unresolved operation", pending["status"] == "needs_reconciliation")
    STATE["mode"] = "normal"
    recovered = run(receipt_dir, "recover", "--operation", pending["id"])["operation"]
    check("known receipt recovers through targeted readback", recovered["status"] == "succeeded" and STATE["creates"] == 2)

    # A lost response deliberately strands the create; no account scan is safe.
    unknown_dir = WORK / "unknown"
    run(unknown_dir, "init")
    unknown_plan = run(unknown_dir, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"))["plan"]
    STATE["mode"] = "lost-receipt"
    failed = subprocess.run([BINARY, "sync", "apply", "--plan", unknown_plan["id"], "--confirm", "--", "--data-dir", str(unknown_dir)], env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in failed.stdout + failed.stderr
    unknown = json.loads(failed.stdout)["operation"]
    check("lost creation receipt is unknown not success", unknown["status"] == "needs_reconciliation" and STATE["creates"] == 3)
    STATE["mode"] = "normal"
    for action, id_flag, value in [("recover", "--operation", unknown["id"]), ("apply", "--plan", unknown_plan["id"])]:
        arguments = [BINARY, "sync", action, id_flag, value]
        if action == "apply":
            arguments.append("--confirm")
        result = subprocess.run(arguments + ["--", "--data-dir", str(unknown_dir)], env=ENV, capture_output=True, text=True, timeout=30)
        assert TOKEN not in result.stdout + result.stderr
        check(action + " after lost receipt does not duplicate create", STATE["creates"] == 3)
    changed = WORK / "changed.html"
    changed.write_text((ROOT / "testdata/synthetic-share.html").read_text().replace("M1 合成分享：格式核对", "M2 changed synthetic title"))
    replanned = subprocess.run([BINARY, "sync", "preview", "--html", str(changed), "--", "--data-dir", str(unknown_dir)], env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in replanned.stdout + replanned.stderr
    if replanned.returncode == 0:
        new_plan = json.loads(replanned.stdout)["plan"]
        result = subprocess.run([BINARY, "sync", "apply", "--plan", new_plan["id"], "--confirm", "--", "--data-dir", str(unknown_dir)], env=ENV, capture_output=True, text=True, timeout=30)
        assert TOKEN not in result.stdout + result.stderr
    check("changed snapshot and new plan cannot bypass unknown creation", STATE["creates"] == 3)
    graph_dir = WORK / "graph"
    run(graph_dir, "init")
    graph_plan = run(graph_dir, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"))["plan"]
    STATE["mode"] = "graph-readback"
    result = subprocess.run([BINARY, "sync", "apply", "--plan", graph_plan["id"], "--confirm", "--", "--data-dir", str(graph_dir)], env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in result.stdout + result.stderr
    graph_operation = json.loads(result.stdout)["operation"]
    check("postread reparenting cannot hide behind identical DFS order", graph_operation["status"] == "needs_reconciliation")
    crash_dir = WORK / "crashed"
    run(crash_dir, "init")
    crash_plan = run(crash_dir, "preview", "--html", str(ROOT / "testdata/synthetic-share.html"))["plan"]
    STATE["mode"] = "block-create"
    process = subprocess.Popen([BINARY, "sync", "apply", "--plan", crash_plan["id"], "--confirm", "--", "--data-dir", str(crash_dir)], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        assert CREATE_STARTED.wait(10), "mock create never started"
    finally:
        process.kill()
        out, err = process.communicate(timeout=10)
        CREATE_RELEASE.set()
    assert TOKEN not in out + err
    STATE["mode"] = "normal"
    offline_env = {k: v for k, v in ENV.items() if not k.startswith("GATE_")}
    pending_result = subprocess.run([BINARY, "sync", "pending", "--", "--data-dir", str(crash_dir)], env=offline_env, capture_output=True, text=True, timeout=30)
    assert pending_result.returncode == 0, pending_result.stderr
    crashed = json.loads(pending_result.stdout)["operations"]
    check("crash before CLI receipt remains discoverable offline", len(crashed) == 1 and crashed[0]["status"] == "needs_reconciliation")
    creates_before = STATE["creates"]
    result = subprocess.run([BINARY, "sync", "recover", "--operation", crashed[0]["id"], "--", "--data-dir", str(crash_dir)], env=ENV, capture_output=True, text=True, timeout=30)
    assert TOKEN not in result.stdout + result.stderr
    check("hard-killed create is not blindly retried", STATE["creates"] == creates_before)
    check("no list scan or delete was used", all(path not in ("/api/v1/chats/", "/api/v1/chats") for _, path in STATE["requests"]))
    check("database and installation files private", all(p.stat().st_mode & 0o077 == 0 for p in WORK.rglob("*.db")))
    print(json.dumps({"passed": len(RESULTS), "checks": RESULTS, "workspace": str(WORK)}, ensure_ascii=False, indent=2))


try:
    main()
finally:
    SERVER.shutdown()
    SERVER.server_close()
