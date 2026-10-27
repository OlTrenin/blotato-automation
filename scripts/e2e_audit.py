#!/usr/bin/env python3
"""Bounded HTTP audit. Run only against a disposable local demo instance.

Writes machine-readable evidence; exit 1 means unexpected behavior or a
hardening gap was found. Some checks exceed the documented demo contract.
No external provider or customer data is used. Created automations are disabled.
"""
import concurrent.futures
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request
import uuid

BASE = os.getenv("AUDIT_BASE_URL", "http://localhost:18081")
TOKEN = os.getenv("API_TOKEN", "local-demo-token-change-me")
RUN = uuid.uuid4().hex[:12]
RESULTS = []
CREATED = []


def call(method, path, body=None, raw=None, token=TOKEN, content_type="application/json", response_headers=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    headers = {"Content-Type": content_type}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=headers)
    try:
        response = urllib.request.urlopen(req, timeout=15)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        if response_headers is not None:
            response_headers.update(response.headers)
        text = response.read().decode()
        try:
            value = json.loads(text)
        except ValueError:
            value = text
        return response.code, value


def list_all(path):
    values = []
    after = ""
    while True:
        headers = {}
        code, page = call("GET", path + "?limit=100&after=" + after, response_headers=headers)
        if code != 200:
            raise RuntimeError(f"List failed: HTTP {code}")
        values.extend(page)
        after = headers.get("X-Next-Cursor", "")
        if not after:
            return values


def record(name, expected, observed, ok):
    RESULTS.append(dict(name=name, expected=expected, observed=observed,
                        verdict="pass" if ok else "finding"))


def automation(**changes):
    return dict(platform="instagram", account_id="account-123", keyword="audit" + RUN,
                comment_reply="Reply", email_prompt="Email?", link="https://example.com", **changes)


def create(value=None, **kwargs):
    code, result = call("POST", "/v1/automations", body=value, **kwargs)
    if code == 201:
        CREATED.append(result["id"])
    return code, result


def comment(suffix, text):
    return dict(event_id=RUN + "e" + suffix, comment_id=RUN + "c" + suffix,
                user_id=RUN + "u" + suffix, post_id="audit-post", platform="instagram",
                account_id="account-123", text=text)


def wait(execution_id, state):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        code, value = call("GET", "/v1/executions/" + execution_id)
        if code == 200 and value["state"] == state:
            return value
        time.sleep(.05)
    raise RuntimeError(f"{execution_id}: did not reach {state}")


def main():
    for token in (None, "wrong-credential"):
        code, value = call("GET", "/v1/automations", token=token)
        record("authentication " + str(token), 401, code, code == 401)
    base = automation()
    for name, raw, wanted in [
        ("unknown field", b'{"tenant_id":"other"}', 400),
        ("multiple objects", b'{} {}', 400),
        ("oversized body", b'{"text":"' + b'x' * 70000 + b'"}', 413),
        ("null object", b'null', 400),
    ]:
        code, _ = call("POST", "/v1/automations", raw=raw)
        record(name, wanted, code, code == wanted)
    foreign = dict(base, account_id="unowned-account")
    code, _ = create(foreign)
    record("unowned account", 404, code, code == 404)
    code, a = create(base)
    assert code == 201, a
    event = comment("parallel", base["keyword"].upper() + "!")
    with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
        replies = list(pool.map(lambda _: call("POST", "/v1/events/comments", event), range(32)))
    ids = {r[1].get("execution_id") for r in replies}
    fresh = sum(not r[1].get("duplicate", True) for r in replies)
    record("32 concurrent identical comments", "one execution, one fresh receipt",
           dict(statuses=sorted({r[0] for r in replies}), executions=len(ids), fresh=fresh),
           all(r[0] == 200 for r in replies) and len(ids) == 1 and fresh == 1)
    execution_id = next(iter(ids))
    wait(execution_id, "WAITING_FOR_EMAIL")
    code, result = call("POST", "/v1/events/comments", dict(event, text="changed"))
    record("changed content replay", 409, code, code == 409)
    code, result = call("POST", "/v1/events/comments", dict(event, event_id=RUN + "alias"))
    record("logical object replay", "duplicate original execution", result,
           code == 200 and result.get("duplicate") and result.get("execution_id") == execution_id)
    # This is a quoted local part containing @, followed by a second @domain.
    # The documented unquoted-mailbox policy must not extract the inner address.
    message = dict(platform="instagram", account_id="account-123", user_id=event["user_id"],
                   event_id=RUN + "email", message_id=RUN + "email", text='"alice@example.com"@example.org')
    code, result = call("POST", "/v1/events/messages", message)
    record("quoted email local part", "invalid_email; stay waiting", result,
           code == 200 and result.get("outcome") == "invalid_email")
    code, observed = call("GET", "/v1/executions/" + execution_id)
    record("quoted email workflow effect", "stay waiting without final link", observed,
           code == 200 and observed["state"] == "WAITING_FOR_EMAIL"
           and not any(a["kind"] == "send_link" for a in observed["actions"]))
    message.update(event_id=RUN + "goodemail", message_id=RUN + "goodemail", text="alice@example.org")
    code, observed = call("POST", "/v1/events/messages", message)
    assert code == 200 and observed["outcome"] == "email_received", observed
    wait(execution_id, "COMPLETED")
    for link in ("https://.", "https://example.com:99999"):
        code, value = create(dict(base, link=link))
        record("invalid destination " + link, 400, dict(status=code, response=value), code == 400)
    raw = json.dumps(base).encode()[:-1] + b',"keyword":"other"}'
    code, value = create(raw=raw)
    record("duplicate JSON member", "400 for ambiguous field", dict(status=code, response=value), code == 400)
    raw = json.dumps(base).encode().replace(b'"Reply"', b'"bad\xfftext"')
    code, value = create(raw=raw)
    record("invalid UTF-8", "400 without silently changing content", dict(status=code, response=value), code == 400)
    code, value = create(base, content_type="Application/JSON")
    record("case insensitive media type", 201, dict(status=code, response=value), code == 201)
    alias = json.dumps(comment("unicode", "unmatched")).encode()[:-1] + ',"poſt_id":"other"}'.encode()
    code, value = call("POST", "/v1/events/comments", raw=alias)
    record("Unicode JSON field alias", 400, dict(status=code, response=value), code == 400)
    code, value = call("GET", "/v1/automations?limit=1")
    record("bounded list pagination", "bounded response",
           dict(status=code, count=len(value)), code == 200 and len(value) <= 1)
    # Bounded burst against a validation-only path: exercises the actual server
    # limiter without creating DB records or putting pressure on the DB pool.
    started = time.monotonic()
    def rate_probe(_):
        headers = {}
        status, _ = call("GET", "/v1/automations/!", response_headers=headers)
        return status, headers.get("Retry-After")
    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
        replies = list(pool.map(rate_probe, range(500)))
    limited = [r for r in replies if r[0] == 429]
    record("HTTP rate limit under bounded burst", "429 with Retry-After; no 5xx",
           dict(requests=500, limited=len(limited), seconds=round(time.monotonic()-started,3),
                statuses=sorted({r[0] for r in replies})),
           bool(limited) and all(r[1] == "1" for r in limited)
           and all(r[0] in (400,429) for r in replies))
    time.sleep(1.1)
    code, value = call("GET", "/v1/automations?limit=1")
    record("rate limit replenishes", 200, code, code == 200)



if __name__ == "__main__":
    try:
        main()
    finally:
        for automation_id in CREATED:
            call("PATCH", "/v1/automations/" + automation_id, {"enabled": False})
        output = Path(os.getenv("AUDIT_OUTPUT", "docs/e2e-audit-fixed-results.json"))
        output.write_text(json.dumps(dict(run=RUN, base_url=BASE, results=RESULTS), indent=2) + "\n")
    print(json.dumps(dict(checks=len(RESULTS), findings=sum(r["verdict"] == "finding" for r in RESULTS),
                          evidence=str(output)), indent=2))
    sys.exit(int(any(r["verdict"] == "finding" for r in RESULTS)))
