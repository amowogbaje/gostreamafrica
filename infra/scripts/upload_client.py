#!/usr/bin/env python3
"""Upload a video through the StreamAfrica API exactly like a browser would (standard library only).

  1. log in                         POST /v1/auth/login
  2. start the upload               POST /v1/creator/uploads         -> part size, part count, first URLs
  3. PUT each part to its URL       straight to object storage (the API never sees the bytes)
  4. ask for more URLs when needed  POST /v1/creator/uploads/{id}/parts
  5. finish                         POST /v1/creator/uploads/{id}/complete

Pause and resume:  --stop-after N  sends N parts and stops, as if the connection dropped.
                   --resume ID     asks the API which parts are stored, and sends only the rest.

Examples (see docs/upload-flow.md for the full walkthrough):
  python3 infra/scripts/upload_client.py --email me@example.com --password '...' --title <title-id> --file film.mp4
  python3 infra/scripts/upload_client.py ... --file film.mp4 --stop-after 2
  python3 infra/scripts/upload_client.py ... --file film.mp4 --resume <upload-id>
  python3 infra/scripts/upload_client.py ... --abort <upload-id>
"""
import argparse
import json
import mimetypes
import os
import sys
import time
import urllib.error
import urllib.request

BATCH = 20  # the API hands out at most 20 part URLs per call


def default_api():
    """http://localhost:<NGINX_HOST_PORT>/api, read from .env when present."""
    port = os.environ.get("NGINX_HOST_PORT")
    env_file = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", ".env")
    if not port and os.path.exists(env_file):
        for line in open(env_file, encoding="utf-8"):
            if line.startswith("NGINX_HOST_PORT="):
                port = line.split("=", 1)[1].strip()
    return f"http://localhost:{port or 8088}/api"


class ApiError(Exception):
    def __init__(self, status, code, message):
        super().__init__(f"HTTP {status} {code}: {message}")
        self.status, self.code, self.message = status, code, message


def call(method, url, token=None, body=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            err = json.loads(raw)["error"]
            raise ApiError(e.code, err.get("code", "?"), err.get("message", "")) from None
        except (ValueError, KeyError):
            raise ApiError(e.code, "?", raw[:200].decode("utf-8", "replace")) from None


def put_part(url, chunk, retries=3):
    """PUT one part. The body length is part of the URL's signature, so it must be exact."""
    last = None
    for attempt in range(1, retries + 1):
        req = urllib.request.Request(url, data=chunk, method="PUT")
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                return resp.headers.get("ETag")
        except urllib.error.HTTPError as e:
            last = f"HTTP {e.code}: {e.read()[:200].decode('utf-8', 'replace')}"
            if e.code in (400, 403):  # an expired or wrong URL will not get better by retrying
                break
        except (urllib.error.URLError, ConnectionError, TimeoutError) as e:
            last = str(e)
        time.sleep(min(2 ** attempt, 10))
    raise RuntimeError(f"part upload failed: {last}")


def human(n):
    for unit in ("B", "KiB", "MiB", "GiB"):
        if n < 1024 or unit == "GiB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024


def read_part(path, number, part_size, size):
    with open(path, "rb") as f:
        f.seek((number - 1) * part_size)
        return f.read(min(part_size, size - (number - 1) * part_size))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--api", default=default_api(), help="API base URL (default: from .env)")
    ap.add_argument("--email", required=True)
    ap.add_argument("--password", required=True)
    ap.add_argument("--title", help="id of a movie you own (make dev-creator prints one)")
    ap.add_argument("--file", help="video file to upload")
    ap.add_argument("--stop-after", type=int, metavar="N", help="stop after sending N parts (simulates a dropped connection)")
    ap.add_argument("--resume", metavar="UPLOAD_ID", help="continue an interrupted upload of the same --file")
    ap.add_argument("--abort", metavar="UPLOAD_ID", help="cancel an unfinished upload")
    ap.add_argument("--no-wait", action="store_true", help="do not poll for the job state after completing")
    a = ap.parse_args()
    api = a.api.rstrip("/")

    _, auth = call("POST", api + "/v1/auth/login", body={"email": a.email, "password": a.password})
    token = auth["access_token"]
    print(f"logged in as {a.email}")

    if a.abort:
        call("DELETE", f"{api}/v1/creator/uploads/{a.abort}", token)
        print(f"upload {a.abort} cancelled")
        return 0

    if not a.file:
        ap.error("--file is required")
    size = os.path.getsize(a.file)
    ctype = mimetypes.guess_type(a.file)[0] or "application/octet-stream"

    if a.resume:
        _, up = call("GET", f"{api}/v1/creator/uploads/{a.resume}", token)
        if up["status"] != "initiated":
            print(f"upload {up['id']} is '{up['status']}': nothing to resume")
            return report(api, token, up, a.no_wait)
        if up["size_bytes"] != size:
            print(f"the file is {size} bytes but upload {up['id']} expects {up['size_bytes']}: not the same file")
            return 1
        stored = {p["part_number"] for p in up.get("stored_parts", [])}
        urls = {}
        print(f"resuming {up['id']}: {len(stored)} of {up['part_count']} parts already stored: {sorted(stored) or 'none'}")
    else:
        if not a.title:
            ap.error("--title is required to start an upload")
        _, created = call("POST", api + "/v1/creator/uploads", token, {
            "title_id": a.title, "filename": os.path.basename(a.file), "content_type": ctype, "size_bytes": size})
        up = created["upload"]
        urls = {p["part_number"]: p["url"] for p in created["parts"]}
        stored = set()
        print(f"upload {up['id']} started: {human(size)} in {up['part_count']} parts of {human(up['part_size_bytes'])}; "
              f"it expires {up['expires_at']}")
        print(f"  to resume after an interruption: --resume {up['id']}")

    part_size, count = up["part_size_bytes"], up["part_count"]
    todo = [n for n in range(1, count + 1) if n not in stored]
    sent = 0
    started = time.time()
    for n in todo:
        if a.stop_after is not None and sent >= a.stop_after:
            print(f"--stop-after {a.stop_after}: stopping here. {len(stored) + sent} of {count} parts are stored.")
            print(f"  resume with: --resume {up['id']}")
            return 0
        if n not in urls:  # presigned URLs are short-lived: ask for a fresh batch right when needed
            batch = [m for m in todo if m >= n and m not in urls][:BATCH]
            _, fresh = call("POST", f"{api}/v1/creator/uploads/{up['id']}/parts", token, {"part_numbers": batch})
            urls.update({p["part_number"]: p["url"] for p in fresh["parts"]})
        chunk = read_part(a.file, n, part_size, size)
        t0 = time.time()
        put_part(urls[n], chunk)
        sent += 1
        rate = len(chunk) / max(time.time() - t0, 1e-6)
        print(f"  part {n}/{count} sent ({human(len(chunk))} at {human(rate)}/s)")
    print(f"all parts stored in {time.time() - started:.1f}s; completing...")

    try:
        _, done = call("POST", f"{api}/v1/creator/uploads/{up['id']}/complete", token, timeout=600)
    except ApiError as e:
        print(f"complete refused: {e}")
        return 1
    return report(api, token, done, a.no_wait)


def report(api, token, up, no_wait):
    print(f"status: {up['status']}")
    if up.get("reject_code"):
        print(f"  rejected: {up['reject_code']}: {up.get('reject_reason', '')}")
        return 1
    job = up.get("job")
    if job:
        print(f"  job {job['id']}: {job['state']}" + (f" ({job['stage']})" if job.get("stage") else ""))
    if not no_wait and up["status"] in ("uploaded", "processing"):
        print("  (video processing is handled by the worker, added in the next step; until then the job stays queued)")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except ApiError as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)
    except (RuntimeError, urllib.error.URLError) as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)
