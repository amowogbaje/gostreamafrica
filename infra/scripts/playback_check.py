#!/usr/bin/env python3
"""Ask the API for signed playback URLs and prove the media front door enforces them (standard library only).

  python3 infra/scripts/playback_check.py --email me@example.com --password '...' --id <title-or-episode-id>

It logs in, calls GET /v1/playback/{id}, then checks through nginx: the manifest, a variant playlist and a ranged
segment load; a tampered token, a token reused for another asset and a directory listing are refused (403);
and an already-expired URL gets 410. The expiry check needs --key (PLAYBACK_SIGNING_KEY) to forge a genuine but old token.
"""
import argparse
import base64
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request


def default_api():
    port = os.environ.get("NGINX_HOST_PORT")
    env_file = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".env")
    key = os.environ.get("PLAYBACK_SIGNING_KEY")
    if os.path.exists(env_file):
        for line in open(env_file, encoding="utf-8"):
            if line.startswith("NGINX_HOST_PORT=") and not port:
                port = line.split("=", 1)[1].strip()
            if line.startswith("PLAYBACK_SIGNING_KEY=") and not key:
                key = line.split("=", 1)[1].strip()
    return f"http://localhost:{port or 8088}/api", key


def fetch(url, headers=None, method="GET", body=None):
    req = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


failed = False


def check(name, cond, detail=""):
    global failed
    failed |= not cond
    print(("ok    " if cond else "FAIL  ") + name + ("" if cond else f"   ({detail})"))


def main():
    api_default, key_default = default_api()
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--api", default=api_default)
    ap.add_argument("--email", required=True)
    ap.add_argument("--password", required=True)
    ap.add_argument("--id", required=True, help="a movie's title id or an episode id")
    ap.add_argument("--key", default=key_default, help="PLAYBACK_SIGNING_KEY (read from .env by default), used to forge an expired token")
    a = ap.parse_args()
    api = a.api.rstrip("/")

    st, _, body = fetch(api + "/v1/auth/login", {"Content-Type": "application/json"}, "POST",
                        json.dumps({"email": a.email, "password": a.password}).encode())
    if st != 200:
        sys.exit(f"login failed: HTTP {st}")
    token = json.loads(body)["access_token"]
    st, h, body = fetch(f"{api}/v1/playback/{a.id}", {"Authorization": "Bearer " + token})
    if st != 200:
        sys.exit(f"playback request refused: HTTP {st} {body[:200].decode('utf-8', 'replace')}")
    pb = json.loads(body)
    print(f"manifest : {pb['manifest_url']}\nexpires  : {pb['expires_at']}   duration: {pb['duration_seconds']}s   subtitles: {[s['language'] for s in pb['subtitles']]}\n")
    check("response is not cacheable", h.get("Cache-Control") == "no-store", h.get("Cache-Control"))

    man = pb["manifest_url"]
    m = re.match(r"(https?://[^/]+)/hls/([^/]+)/(\d+)/([0-9a-f-]{36})/(.+)$", man)
    base, tok, exp, asset, _ = m.groups()

    def url(t, e, a_, f):
        return f"{base}/hls/{t}/{e}/{a_}/{f}"

    st, h, body = fetch(man)
    check("manifest loads", st == 200 and body.startswith(b"#EXTM3U"), st)
    variants = [l for l in body.decode().splitlines() if l and not l.startswith("#")]
    print("          qualities:", [v.split("/")[0] for v in variants])
    v0 = variants[0]
    st, _, body = fetch(url(tok, exp, asset, v0))
    check("a variant playlist loads with the SAME token (it rides in the path)", st == 200, st)
    seg = next(l for l in body.decode().splitlines() if l.endswith(".m4s"))
    st, h, _ = fetch(url(tok, exp, asset, os.path.dirname(v0) + "/" + seg), {"Range": "bytes=0-99"})
    check("a segment answers a range request (206)", st == 206, st)

    flip = ("A" if tok[0] != "A" else "B") + tok[1:]  # (the last character holds only padding bits, so flipping it changes nothing)
    check("tampered token is refused (403)", fetch(url(flip, exp, asset, "master.m3u8"))[0] == 403)
    check("a longer expiry with the old token is refused (403)", fetch(url(tok, int(exp) + 86400, asset, "master.m3u8"))[0] == 403)
    other = asset[:-1] + ("0" if asset[-1] != "0" else "1")
    check("the token cannot be reused for another asset (403)", fetch(url(tok, exp, other, "master.m3u8"))[0] == 403)
    check("directory listing is refused (403)", fetch(url(tok, exp, asset, os.path.dirname(v0) + "/"))[0] == 403)
    check("unsigned URL is refused (403)", fetch(f"{base}/hls/{asset}/master.m3u8")[0] == 403)
    if a.key:
        past = int(time.time()) - 60
        forged = base64.urlsafe_b64encode(hashlib.md5(f"{past}{asset} {a.key}".encode()).digest()).decode().rstrip("=")
        check("a genuine but expired token gets 410", fetch(url(forged, past, asset, "master.m3u8"))[0] == 410)
    else:
        print("skip  expiry check (pass --key to run it)")
    print("\nALL GOOD" if not failed else "\nSOME CHECKS FAILED")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
