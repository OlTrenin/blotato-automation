#!/usr/bin/env python3
"""Crash and DB-outage checks, restricted to the separate audit Compose project."""
import json
import os
from pathlib import Path
import subprocess
import time

import e2e_audit as audit

if os.getenv("COMPOSE_PROJECT_NAME") != "blotato-e2e-audit":
    raise SystemExit("Requires COMPOSE_PROJECT_NAME=blotato-e2e-audit")
if audit.BASE != "http://localhost:18081":
    raise SystemExit("Requires the disposable audit endpoint http://localhost:18081")


def compose(*args):
    subprocess.run(["docker", "compose", *args], check=True, timeout=45,
                   stdout=subprocess.DEVNULL)


def ready():
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            if audit.call("GET", "/readyz")[0] == 200:
                return
        except OSError:
            pass
        time.sleep(.1)
    raise RuntimeError("Service did not recover readiness")


evidence = {}
try:
    code, automation = audit.create(audit.automation())
    assert code == 201
    assert audit.call("POST", "/v1/simulator/failures", dict(
        kind="request_email", mode="after_success", count=1))[0] == 200
    event = audit.comment("crash", automation["keyword"])
    code, result = audit.call("POST", "/v1/events/comments", event)
    assert code == 200
    execution_id = result["execution_id"]
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        code, execution = audit.call("GET", "/v1/executions/" + execution_id)
        pending = [a for a in execution["actions"] if a["kind"] == "request_email"
                   and a["status"] == "pending" and a["attempts"] == 1]
        if pending:
            break
        time.sleep(.005)
    else:
        raise RuntimeError("Did not observe the after-success retry boundary")
    compose("kill", "-s", "SIGKILL", "app")
    # With the app stopped, confirm the exact crash boundary from durable data.
    durable = subprocess.run([
        "docker", "compose", "exec", "-T", "db", "psql", "-U", "blotato", "-d", "blotato",
        "-At", "-c", "SELECT json_build_object('status',status,'attempts',attempts) "
        "FROM actions WHERE execution_id='" + execution_id + "' AND kind='request_email'"
    ], check=True, capture_output=True, text=True, timeout=10)
    crash_boundary = json.loads(durable.stdout)
    assert crash_boundary == {"status": "pending", "attempts": 1}, crash_boundary
    compose("up", "-d", "app")
    ready()
    recovered = audit.wait(execution_id, "WAITING_FOR_EMAIL")
    request_action = next(a for a in recovered["actions"] if a["kind"] == "request_email")
    effects = [o for o in audit.list_all("/v1/simulator/outbound")
               if o["execution_id"] == execution_id]
    assert request_action["attempts"] == 2, request_action
    assert [o["kind"] for o in effects] == ["reply_comment", "request_email"], effects
    evidence["sigkill_after_remote_success"] = dict(
        execution_id=execution_id, state=recovered["state"], attempts=request_action["attempts"],
        effects=len(effects), crash_boundary=crash_boundary, verdict="pass")
    compose("stop", "db")
    code, value = audit.call("GET", "/readyz")
    assert code == 503, (code, value)
    assert audit.call("GET", "/healthz")[0] == 200
    message = dict(platform="instagram", account_id="account-123", user_id=event["user_id"],
                   event_id=audit.RUN + "outage", message_id=audit.RUN + "outage",
                   text="recovery@example.com")
    code, value = audit.call("POST", "/v1/events/messages", message)
    assert code == 500 and value == {"error": "internal error"}, (code, value)
    evidence["database_outage"] = dict(readyz=503, healthz=200, ingestion=code,
                                       response=value, verdict="pass")
    compose("up", "-d", "--wait", "db")
    ready()
    assert audit.call("POST", "/v1/events/messages", message)[1]["outcome"] == "email_received"
    final = audit.wait(execution_id, "COMPLETED")
    replay = audit.call("POST", "/v1/events/messages", message)[1]
    assert replay["duplicate"]
    effects = [o for o in audit.list_all("/v1/simulator/outbound")
               if o["execution_id"] == execution_id]
    assert len(effects) == 3
    evidence["database_recovery_and_retry"] = dict(state=final["state"], effects=len(effects),
                                                 duplicate=replay["duplicate"], verdict="pass")
finally:
    compose("up", "-d", "--wait", "db", "app")
    ready()
    for automation_id in audit.CREATED:
        audit.call("PATCH", "/v1/automations/" + automation_id, {"enabled": False})
    Path("docs/e2e-recovery-fixed-results.json").write_text(json.dumps(evidence, indent=2) + "\n")

print(json.dumps(evidence, indent=2))
