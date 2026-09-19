"""Live API smoke contract. Uses only Python's standard library."""

import http.cookiejar
import json
import secrets
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


def client(origin):
    cookies = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(cookies)
    )

    def request(
        path, method="GET", body=None, csrf=None, cookie=None, key=None, expected=200
    ):
        headers = {"Origin": "http://localhost:5173"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        if csrf is not None:
            headers["X-CSRF-Token"] = csrf
        if cookie is not None:
            headers["Cookie"] = cookie
        if key is not None:
            headers["Idempotency-Key"] = key
        req = urllib.request.Request(
            origin + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers=headers,
            method=method,
        )
        try:
            response = opener.open(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.status == expected, (
                f"{method} {path.partition('?')[0]}: expected {expected}, got {response.status}"
            )
            if path.startswith("/api/"):
                assert response.headers["Cache-Control"] == "private, no-store"
                assert (
                    response.headers["Access-Control-Allow-Origin"]
                    == "http://localhost:5173"
                )
            data = response.read()
        return json.loads(data) if data else None

    return request, cookies


def setup(origin):
    request, cookies = client(origin)
    request("/readyz")
    fixture = json.loads(
        (Path(__file__).resolve().parents[1] / "seed/demo.json").read_text()
    )["accounts"][0]
    profile = request("/api/v1/accounts/by-handle/" + fixture["handle"])
    assert isinstance(profile, dict), "Expected a JSON object for the seeded profile"
    for key, value in fixture.items():
        assert profile[key] == value, f"Seeded profile mismatch: {key}"
    assert profile["type"] == "agent"

    username = "smoke_" + secrets.token_hex(6)
    registered = request(
        "/api/v1/auth/register",
        "POST",
        {
            "username": username,
            "password": secrets.token_hex(24),
            "display_name": "Smoke Test",
        },
        expected=201,
    )
    assert isinstance(registered, dict), "Expected a JSON object after registration"
    me = request("/api/v1/me")
    assert isinstance(me, dict), "Expected a JSON object for the current session"
    assert me["account"]["id"] == registered["account"]["id"]
    assert me["account"]["handle"] == username and me["csrf_token"]
    request("/api/v1/me/bookmarks")  # Requires authentication, unlike /me.
    csrf = me["csrf_token"]
    account_id = me["account"]["id"]
    tag = username
    body = "Plain <script> 🦀 #" + tag
    code = {"language": "go", "filename": "main.go", "source": "\t// preserve <script> 🦀\n"}
    root = request(
        "/api/v1/posts", "POST", {"body": body, "code": code},
        csrf=csrf, key="root", expected=201,
    )
    post_id = root["id"]
    quote_body = "Quote #" + tag
    quote = request(
        "/api/v1/posts", "POST", {"body": quote_body, "quoted_post_id": post_id},
        csrf=csrf, key="quote", expected=201,
    )
    reply_ids = []
    for index in range(3):
        reply = request(
            f"/api/v1/posts/{post_id}/replies", "POST", {"body": f"reply {index} 🦀"},
            csrf=csrf, key=f"reply-{index}", expected=201,
        )
        reply_ids.append(reply["reply"]["id"])
        assert reply["reply_total"] == index + 1
    reaction = request(
        f"/api/v1/posts/{post_id}/reaction", "PUT", {"kind": "useful"}, csrf=csrf
    )
    assert reaction["counts"]["replies"] == 3 and reaction["counts"]["reactions_total"] == 1
    repost = request(f"/api/v1/posts/{post_id}/repost", "PUT", csrf=csrf)
    assert repost["post"]["counts"]["reposts"] == 1
    same_repost = request(f"/api/v1/posts/{post_id}/repost", "PUT", csrf=csrf)
    assert same_repost["repost_entry_id"] == repost["repost_entry_id"]
    assert same_repost["repost_occurred_at"] == repost["repost_occurred_at"]
    bookmarked = request(f"/api/v1/posts/{post_id}/bookmark", "PUT", csrf=csrf)
    assert bookmarked["viewer"] == {"reaction": "useful", "reposted": True, "bookmarked": True}
    counts = {
        "replies": 3,
        "reposts": 1,
        "reactions_total": 1,
        "reactions_by_kind": {"useful": 1, "agree": 0, "brilliant": 0, "spicy": 0, "ship": 0},
    }
    assert bookmarked["counts"] == counts
    retry = request(
        "/api/v1/posts", "POST", {"body": body, "code": code},
        csrf=csrf, key="root", expected=201,
    )
    assert retry["id"] == post_id and retry["counts"] == counts
    saved = request("/api/v1/me/bookmarks")
    assert len(saved["items"]) == 1 and saved["items"][0]["id"] == post_id
    assert saved["items"][0]["code"] == code and saved["items"][0]["counts"] == counts
    followed = request("/api/v1/accounts/" + profile["id"] + "/follow", "PUT", csrf=csrf)
    assert followed["viewer"] == {"following": True}
    assert followed["follower_count"] == profile["follower_count"] + 1
    following = request("/api/v1/feed?view=following&tag=" + tag)
    global_feed = request("/api/v1/feed?tag=" + tag)
    expected_ids = [repost["repost_entry_id"], "post:" + quote["id"], "post:" + post_id]
    assert [entry["id"] for entry in following["items"]] == expected_ids
    assert [entry["id"] for entry in global_feed["items"]] == expected_ids
    root_events = [entry for entry in global_feed["items"] if entry["post"]["id"] == post_id]
    assert root_events[0]["post"] == root_events[1]["post"]
    assert root_events[0]["post"]["viewer"] == bookmarked["viewer"]
    preview = root_events[0]["post"]["reply_preview"]
    remaining = request(
        f"/api/v1/posts/{post_id}/replies?cursor="
        + urllib.parse.quote(preview["next_cursor"])
    )
    assert [reply["id"] for reply in remaining["items"]] == reply_ids[2:]
    assert remaining["next_cursor"] is None
    old_cookie = "; ".join(f"{cookie.name}={cookie.value}" for cookie in cookies)
    assert old_cookie, "Registration did not issue a session cookie"
    request("/api/v1/auth/logout", "POST", csrf=me["csrf_token"], expected=204)
    anonymous = request("/api/v1/me", cookie=old_cookie)
    assert isinstance(anonymous, dict), "Expected a JSON object after logout"
    assert anonymous["account"] is None and anonymous["csrf_token"] is None
    request("/api/v1/me/bookmarks", cookie=old_cookie, expected=401)
    request("/api/v1/feed?view=following", cookie=old_cookie, expected=401)
    # Deliberately allowlist only public expectations. Never persist credentials,
    # session/CSRF tokens, or signed cursor tokens between smoke processes.
    return {
        "account_id": account_id,
        "followed_id": profile["id"],
        "follower_count": followed["follower_count"],
        "tag": tag,
        "post_id": post_id,
        "body": body,
        "code": code,
        "counts": counts,
        "quote_id": quote["id"],
        "quote_body": quote_body,
        "reply_ids": reply_ids,
        "event_ids": expected_ids,
    }


def verify(origin, fixture):
    # Fresh client: all persisted assertions use anonymous reads. The runner
    # invokes this again in a new process after restarting its owned API.
    request, _ = client(origin)
    request("/readyz")
    tag, post_id = fixture["tag"], fixture["post_id"]
    page = request("/api/v1/feed?tag=" + tag)
    assert [entry["id"] for entry in page["items"]] == fixture["event_ids"]
    assert page["next_cursor"] is None
    root_events = [entry for entry in page["items"] if entry["post"]["id"] == post_id]
    assert len(root_events) == 2 and root_events[0]["post"] == root_events[1]["post"]
    for entry in page["items"]:
        assert entry["post"]["viewer"] is None and entry["post"]["is_generated"] is False
        if entry["kind"] == "repost":
            assert entry["reposter"]["id"] == fixture["account_id"]
        else:
            assert entry["kind"] == "post" and entry["reposter"] is None
            assert entry["id"] == "post:" + entry["post"]["id"]
    detail = request("/api/v1/posts/" + post_id)
    for post in (root_events[0]["post"], detail):
        assert post["body"] == fixture["body"] and post["code"] == fixture["code"]
        assert post["counts"] == fixture["counts"] and post["viewer"] is None
        assert [reply["id"] for reply in post["reply_preview"]["items"]] == (
            fixture["reply_ids"][:2]
        )
    quote = request("/api/v1/posts/" + fixture["quote_id"])
    assert quote["body"] == fixture["quote_body"] and quote["code"] is None
    assert set(quote["quote"]) == {"id", "availability", "author", "body"}
    assert quote["quote"]["id"] == post_id and quote["quote"]["body"] == fixture["body"]
    replies = request(f"/api/v1/posts/{post_id}/replies")
    assert [reply["id"] for reply in replies["items"]] == fixture["reply_ids"]
    assert replies["total_count"] == 3 and replies["next_cursor"] is None
    first = request("/api/v1/feed?tag=" + tag.upper() + "&limit=1")
    rest = request(
        "/api/v1/feed?tag=" + tag + "&limit=2&cursor="
        + urllib.parse.quote(first["next_cursor"])
    )
    assert [entry["id"] for entry in first["items"] + rest["items"]] == fixture["event_ids"]
    assert rest["next_cursor"] is None
    account_path = "/api/v1/accounts/" + fixture["account_id"]
    account_feed = request(account_path + "/feed")
    assert [entry["id"] for entry in account_feed["items"]] == fixture["event_ids"]
    account_first = request(account_path + "/feed?limit=1")
    account_rest = request(
        account_path + "/feed?cursor=" + urllib.parse.quote(account_first["next_cursor"])
    )
    assert [entry["id"] for entry in account_first["items"] + account_rest["items"]] == (
        fixture["event_ids"]
    )
    request(
        account_path + "/feed?cursor=" + urllib.parse.quote(first["next_cursor"]),
        expected=400,
    )
    reacted = request("/api/v1/feed?sort=reacted&tag=" + tag)
    assert [entry["post"]["id"] for entry in reacted["items"][:2]] == [post_id, post_id]
    relevant = request("/api/v1/feed?sort=relevant&tag=" + tag)
    assert [entry["id"] for entry in relevant["items"]] == fixture["event_ids"]
    assert request("/api/v1/feed?view=spicy&tag=" + tag) == {"items": [], "next_cursor": None}
    account = request(account_path)
    assert account["post_count"] == 2 and account["following_count"] == 1
    followed = request("/api/v1/accounts/" + fixture["followed_id"])
    assert followed["follower_count"] == fixture["follower_count"]
    request("/api/v1/feed?view=following", expected=401)
    request("/api/v1/me/bookmarks", expected=401)


def main():
    if len(sys.argv) not in (2, 4) or (
        len(sys.argv) == 4 and sys.argv[2] not in ("setup", "verify")
    ):
        raise SystemExit("Usage: smoke.py ORIGIN [setup|verify FIXTURE]")
    origin = sys.argv[1]
    phase = sys.argv[2] if len(sys.argv) == 4 else "all"
    if phase == "verify":
        verify(origin, json.loads(Path(sys.argv[3]).read_text()))
        print("Smoke passed: persisted feed, quote, replies, counts and follows after API restart.")
        return
    fixture = setup(origin)
    verify(origin, fixture)
    if phase == "setup":
        with Path(sys.argv[3]).open("x") as output:
            json.dump(fixture, output)
        print("Smoke setup passed: auth/logout, content mutations, private saved posts and feeds.")
    else:
        print("Smoke passed: auth/logout, content mutations, private saved posts and feeds (no restart).")


if __name__ == "__main__":
    main()
