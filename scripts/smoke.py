"""Live API smoke contract. Uses only Python's standard library."""

import http.cookiejar
import json
import secrets
import sys
import urllib.error
import urllib.request
from pathlib import Path


def main():
    origin = sys.argv[1]
    cookies = http.cookiejar.CookieJar()
    client = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(cookies)
    )

    def request(path, method="GET", body=None, csrf=None, cookie=None, expected=200):
        headers = {"Origin": "http://localhost:5173"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        if csrf is not None:
            headers["X-CSRF-Token"] = csrf
        if cookie is not None:
            headers["Cookie"] = cookie
        req = urllib.request.Request(
            origin + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers=headers,
            method=method,
        )
        try:
            response = client.open(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            assert response.status == expected, (
                f"{method} {path}: expected {expected}, got {response.status}"
            )
            data = response.read()
        return json.loads(data) if data else None

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
    old_cookie = "; ".join(f"{cookie.name}={cookie.value}" for cookie in cookies)
    assert old_cookie, "Registration did not issue a session cookie"
    request("/api/v1/auth/logout", "POST", csrf=me["csrf_token"], expected=204)
    anonymous = request("/api/v1/me", cookie=old_cookie)
    assert isinstance(anonymous, dict), "Expected a JSON object after logout"
    assert anonymous["account"] is None and anonymous["csrf_token"] is None
    request("/api/v1/me/bookmarks", cookie=old_cookie, expected=401)
    print(
        "Smoke passed: readiness, seeded profile, registration, authenticated access, logout invalidation."
    )


if __name__ == "__main__":
    main()
