#!/usr/bin/env python3
"""Exercise the running HTTP service and PostgreSQL-backed workflow."""

import argparse
import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.getenv("API_BASE_URL", "http://localhost:8080"))
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("--restart", action="store_true", help="restart the Compose app while waiting")
    parser.add_argument("--failure", nargs="?", const="transient", choices=["transient", "after_success"],
                        help="fail one email-request attempt; run demos sequentially")
    args = parser.parse_args()
    token = os.getenv("API_TOKEN", "local-demo-token-change-me")
    account = os.getenv("ACCOUNT_ID", "account-123")
    base = args.base_url.rstrip("/")

    def call(method, path, body=None):
        headers = {"Authorization": "Bearer " + token}
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=5) as response:
                return json.load(response)
        except urllib.error.HTTPError as exc:
            raise RuntimeError(f"{method} {path}: HTTP {exc.code}: "
                               + exc.read().decode(errors="replace")) from exc

    def ready():
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            try:
                if call("GET", "/readyz").get("status") == "ready":
                    return
            except (RuntimeError, urllib.error.URLError, OSError, http.client.HTTPException):
                pass
            time.sleep(0.1)
        raise RuntimeError("service did not become ready before timeout")

    ready()
    run = uuid.uuid4().hex[:12]
    # A unique whole token keeps earlier demo definitions from winning a match.
    keyword = "pricing" + run
    prompt = "What is your email address?"
    link = "https://example.com/pricing"
    automation = call("POST", "/v1/automations", {
        "platform": "instagram", "account_id": account, "keyword": keyword,
        "comment_reply": "Sent you a DM!", "email_prompt": prompt, "link": link,
    })
    if args.failure:
        call("POST", "/v1/simulator/failures", {
            "kind": "request_email", "mode": args.failure, "count": 1,
        })
    comment = {
        "event_id": "demo-comment-event-" + run, "platform": "instagram",
        "account_id": account, "post_id": "demo-post-" + run,
        "comment_id": "demo-comment-" + run, "user_id": "demo-user-" + run,
        "text": keyword,
    }
    result = call("POST", "/v1/events/comments", comment)
    if result.get("outcome") != "created" or not result.get("execution_id"):
        raise RuntimeError("matching comment did not create an execution")
    execution_id = result["execution_id"]
    path = "/v1/executions/" + execution_id

    def state_is(wanted):
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            execution = call("GET", path)
            if execution["state"] == wanted:
                return execution
            if execution["state"] in ("FAILED", "EXPIRED"):
                raise RuntimeError(f"execution ended in {execution['state']}")
            time.sleep(0.05)
        raise RuntimeError(f"execution did not reach {wanted} before timeout")

    def effects():
        observed = []
        after = ""
        while True:
            req = urllib.request.Request(base + "/v1/simulator/outbound?limit=100&after=" + after,
                                         headers={"Authorization": "Bearer " + token})
            with urllib.request.urlopen(req, timeout=5) as response:
                observed.extend(json.load(response))
                after = response.headers.get("X-Next-Cursor", "")
            if not after:
                return [effect for effect in observed if effect["execution_id"] == execution_id]

    execution = state_is("WAITING_FOR_EMAIL")
    initial = effects()
    if [(o["kind"], o["body"]) for o in initial] != [
        ("reply_comment", "Sent you a DM!"), ("request_email", prompt)
    ]:
        raise RuntimeError("initial reply and email request were not delivered in order")
    if args.failure:
        request_action = next(a for a in execution["actions"] if a["kind"] == "request_email")
        if request_action["attempts"] < 2:
            raise RuntimeError("configured delivery failure was not retried")
    print(f"Execution {execution_id}: reply and email request delivered; waiting durably")

    invalid = {
        "event_id": "demo-invalid-event-" + run, "platform": "instagram",
        "account_id": account, "user_id": comment["user_id"],
        "message_id": "demo-invalid-message-" + run, "text": "No email yet",
    }
    if call("POST", "/v1/events/messages", invalid)["outcome"] != "invalid_email":
        raise RuntimeError("invalid email did not retain the waiting conversation")
    if call("GET", path)["state"] != "WAITING_FOR_EMAIL" or len(effects()) != 2:
        raise RuntimeError("invalid email advanced the workflow or sent a message")

    if args.restart:
        subprocess.run(["docker", "compose", "restart", "app"], check=True,
                       cwd=Path(__file__).resolve().parents[1], timeout=60)
        ready()
        if call("GET", path)["state"] != "WAITING_FOR_EMAIL":
            raise RuntimeError("waiting state did not survive process restart")
        print("Compose app restarted; persisted waiting state recovered")

    valid = {
        **invalid, "event_id": "demo-valid-event-" + run,
        "message_id": "demo-valid-message-" + run, "text": "My email is hello@example.com",
    }
    if call("POST", "/v1/events/messages", valid)["outcome"] != "email_received":
        raise RuntimeError("valid email did not resume the execution")
    final = state_is("COMPLETED")
    observed = effects()
    if [(o["kind"], o["body"]) for o in observed] != [
        ("reply_comment", "Sent you a DM!"), ("request_email", prompt), ("send_link", link)
    ]:
        raise RuntimeError("completed execution does not have exactly the three expected effects")
    if not final["email_received"] or "hello@example.com" in json.dumps(final):
        raise RuntimeError("email receipt inspection is incorrect or reveals the email")

    for route, event in [("comments", comment), ("messages", invalid), ("messages", valid)]:
        replay = call("POST", "/v1/events/" + route, event)
        if not replay["duplicate"] or replay["execution_id"] != execution_id:
            raise RuntimeError("replayed event did not resolve to its original logical result")
    if call("GET", path)["state"] != "COMPLETED" or len(effects()) != 3:
        raise RuntimeError("duplicate events changed the completed execution")
    call("PATCH", "/v1/automations/" + automation["id"], {"enabled": False})
    print(json.dumps({"execution_id": execution_id, "state": "COMPLETED",
                      "outbound_effects": 3, "duplicate_replays": "verified",
                      "restart": args.restart, "failure_mode": args.failure}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, urllib.error.URLError, OSError, subprocess.SubprocessError) as exc:
        print("Demo failed: " + str(exc), file=sys.stderr)
        sys.exit(1)
