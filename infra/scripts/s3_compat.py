#!/usr/bin/env python3
"""S3 compatibility check for the object store behind StreamAfrica (SeaweedFS locally).

Proves, against the real server, every storage feature Phase 2 relies on:
multipart upload (+ abort + the 24 h abandoned-upload sweep), presigned URLs (PUT, GET, part
upload, expiry, tampering), anonymous-access rules per bucket, range requests, browser CORS
(including ETag exposure, which the browser needs to finish a multipart upload), metadata
round-trip, prefix listing and batch delete.

Subcommands
  check    run every test against S3_ENDPOINT (default)
  seed     put _smoke/test.txt in the public bucket (used by `make smoke`)
  presign  print presigned URLs for S3_PUBLIC_ENDPOINT, so the host can test the browser path

Configuration comes from environment variables (see .env.example). The tests touch only the
prefix `_s3check/` and remove everything they create.
"""
import hashlib
import json
import os
import sys
import time
import uuid

import boto3
import requests
from botocore.client import Config
from botocore.exceptions import ClientError

ENDPOINT = os.environ["S3_ENDPOINT"]
PUBLIC_ENDPOINT = os.environ.get("S3_PUBLIC_ENDPOINT", ENDPOINT)
ACCESS = os.environ["S3_ACCESS_KEY"]
SECRET = os.environ["S3_SECRET_KEY"]
REGION = os.environ.get("S3_REGION", "us-east-1")
RAW = os.environ.get("S3_BUCKET_RAW", "media-raw")
HLS = os.environ.get("S3_BUCKET_HLS", "media-hls")
PUB = os.environ.get("S3_BUCKET_PUBLIC", "media-public")
ORIGINS = [o.strip() for o in os.environ.get("S3_ALLOWED_ORIGINS", "").split(",") if o.strip()]
BAD_ORIGIN = "http://not-allowed.example"
MIB = 1024 * 1024
RUN = "_s3check/" + uuid.uuid4().hex[:8] + "/"
TIMEOUT = 30


def make_client(endpoint, access=ACCESS, secret=SECRET, **cfg):
    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=access,
        aws_secret_access_key=secret,
        region_name=REGION,
        config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, **cfg),
    )


# ---------------------------------------------------------------- tiny test harness
results = []


def check(name):
    def wrap(fn):
        def run():
            try:
                detail = fn()
                results.append((True, name, detail or ""))
                print(f"ok    {name}" + (f"  ({detail})" if detail else ""), flush=True)
            except AssertionError as e:
                results.append((False, name, str(e)))
                print(f"FAIL  {name}: {e}", flush=True)
            except Exception as e:  # noqa: BLE001 - report anything unexpected as a failure
                results.append((False, name, repr(e)))
                print(f"FAIL  {name}: {type(e).__name__}: {e}", flush=True)

        run.__name__ = fn.__name__
        return run

    return wrap


def expect(cond, msg):
    if not cond:
        raise AssertionError(msg)


def status_of(fn):
    """Return the HTTP status of a botocore call (200 on success, the error status otherwise)."""
    try:
        fn()
        return 200
    except ClientError as e:
        return e.response["ResponseMetadata"]["HTTPStatusCode"]


def acao(resp):
    return resp.headers.get("Access-Control-Allow-Origin")


s3 = make_client(ENDPOINT)


def key(name):
    return RUN + name


# ---------------------------------------------------------------- tests
@check("buckets exist and the application credentials work")
def t_buckets():
    for b in (RAW, HLS, PUB):
        s3.head_bucket(Bucket=b)


@check("wrong credentials are rejected")
def t_bad_creds():
    bad = make_client(ENDPOINT, "not-a-key", "not-a-secret")
    st = status_of(lambda: bad.list_objects_v2(Bucket=RAW, MaxKeys=1))
    expect(st == 403, f"expected 403, got {st}. If this is 200 the S3 config file was not loaded and ANY key is accepted")


@check("metadata round-trip: Content-Type and Cache-Control survive (HLS playlist + segment)")
def t_metadata():
    cases = [
        ("a/master.m3u8", b"#EXTM3U\n", "application/vnd.apple.mpegurl", "public, max-age=2"),
        ("a/seg0.ts", os.urandom(2048), "video/mp2t", "public, max-age=31536000, immutable"),
    ]
    for name, body, ctype, cc in cases:
        s3.put_object(Bucket=HLS, Key=key(name), Body=body, ContentType=ctype, CacheControl=cc)
        h = s3.head_object(Bucket=HLS, Key=key(name))
        expect(h["ContentType"] == ctype, f"{name}: Content-Type {h['ContentType']!r} != {ctype!r}")
        expect(h.get("CacheControl") == cc, f"{name}: Cache-Control {h.get('CacheControl')!r} != {cc!r}")


@check("multipart upload: 3 parts, list parts, list uploads, complete, bytes identical")
def t_multipart():
    k = key("multi.bin")
    uid = s3.create_multipart_upload(Bucket=RAW, Key=k, ContentType="video/mp4")["UploadId"]
    sizes = [5 * MIB, 5 * MIB, 1234567]  # all parts but the last must be >= 5 MiB
    sha, parts = hashlib.sha256(), []
    for i, n in enumerate(sizes, 1):
        body = os.urandom(n)
        sha.update(body)
        etag = s3.upload_part(Bucket=RAW, Key=k, UploadId=uid, PartNumber=i, Body=body)["ETag"]
        parts.append({"PartNumber": i, "ETag": etag})
    listed = [p["PartNumber"] for p in s3.list_parts(Bucket=RAW, Key=k, UploadId=uid)["Parts"]]
    expect(listed == [1, 2, 3], f"list_parts returned {listed}")
    ups = s3.list_multipart_uploads(Bucket=RAW, Prefix=RUN).get("Uploads", [])
    expect(any(u["UploadId"] == uid for u in ups), "in-progress upload missing from list_multipart_uploads")
    done = s3.complete_multipart_upload(Bucket=RAW, Key=k, UploadId=uid, MultipartUpload={"Parts": parts})
    expect(done["ETag"].strip('"').endswith("-3"), f"unexpected final ETag {done['ETag']}")
    obj = s3.get_object(Bucket=RAW, Key=k)
    body = obj["Body"].read()
    expect(len(body) == sum(sizes), f"size {len(body)} != {sum(sizes)}")
    expect(hashlib.sha256(body).digest() == sha.digest(), "downloaded bytes differ from uploaded bytes")
    expect(obj["ContentType"] == "video/mp4", f"Content-Type {obj['ContentType']!r}")


@check("multipart abort leaves nothing behind")
def t_abort():
    k = key("abort.bin")
    uid = s3.create_multipart_upload(Bucket=RAW, Key=k)["UploadId"]
    s3.upload_part(Bucket=RAW, Key=k, UploadId=uid, PartNumber=1, Body=os.urandom(5 * MIB))
    s3.abort_multipart_upload(Bucket=RAW, Key=k, UploadId=uid)
    left = [u for u in s3.list_multipart_uploads(Bucket=RAW, Prefix=RUN).get("Uploads", []) if u["Key"] == k]
    expect(not left, "aborted upload still listed")


@check("abandoned-upload sweep works (list uploads with 'Initiated', abort the old ones)")
def t_sweep():
    k = key("abandoned.bin")
    s3.create_multipart_upload(Bucket=RAW, Key=k)
    ups = [u for u in s3.list_multipart_uploads(Bucket=RAW, Prefix=RUN).get("Uploads", []) if u["Key"] == k]
    expect(len(ups) == 1 and ups[0].get("Initiated"), "upload or its Initiated timestamp missing")
    age = time.time() - ups[0]["Initiated"].timestamp()
    expect(-60 < age < 120, f"Initiated timestamp looks wrong (age {age:.0f}s)")
    # The real cleanup job aborts uploads older than 24h. Here we abort with a 0s cutoff.
    for u in ups:
        s3.abort_multipart_upload(Bucket=RAW, Key=u["Key"], UploadId=u["UploadId"])
    expect(not [u for u in s3.list_multipart_uploads(Bucket=RAW, Prefix=RUN).get("Uploads", []) if u["Key"] == k],
           "sweep left the upload in place")
    return "lifecycle rules not needed: the API/worker runs this sweep itself"


@check("presigned PUT/GET: content-type is enforced, tampering and expiry are rejected")
def t_presigned():
    k = key("presigned.bin")
    put = s3.generate_presigned_url("put_object", Params={"Bucket": RAW, "Key": k, "ContentType": "video/mp4"}, ExpiresIn=900)
    r = requests.put(put, data=b"hello", headers={"Content-Type": "video/mp4"}, timeout=TIMEOUT)
    expect(r.status_code == 200, f"presigned PUT -> {r.status_code}")
    r = requests.put(put, data=b"hello", headers={"Content-Type": "text/plain"}, timeout=TIMEOUT)
    expect(r.status_code == 403, f"PUT with a different Content-Type should be 403, got {r.status_code}")
    get = s3.generate_presigned_url("get_object", Params={"Bucket": RAW, "Key": k}, ExpiresIn=900)
    r = requests.get(get, timeout=TIMEOUT)
    expect(r.status_code == 200 and r.content == b"hello", f"presigned GET -> {r.status_code}")
    bad = requests.get(get.replace("presigned.bin", "other.bin"), timeout=TIMEOUT)
    expect(bad.status_code == 403, f"tampered URL should be 403, got {bad.status_code}")
    short = s3.generate_presigned_url("get_object", Params={"Bucket": RAW, "Key": k}, ExpiresIn=1)
    time.sleep(3)
    exp = requests.get(short, timeout=TIMEOUT)
    expect(exp.status_code == 403, f"expired URL should be 403, got {exp.status_code}")


@check("presigned multipart part upload + complete (what the browser does)")
def t_presigned_part():
    k = key("presigned-multi.bin")
    uid = s3.create_multipart_upload(Bucket=RAW, Key=k)["UploadId"]
    url = s3.generate_presigned_url(
        "upload_part", Params={"Bucket": RAW, "Key": k, "UploadId": uid, "PartNumber": 1}, ExpiresIn=900)
    r = requests.put(url, data=os.urandom(5 * MIB), timeout=TIMEOUT)
    expect(r.status_code == 200 and r.headers.get("ETag"), f"part PUT -> {r.status_code}, ETag={r.headers.get('ETag')}")
    s3.complete_multipart_upload(Bucket=RAW, Key=k, UploadId=uid,
                                 MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": r.headers["ETag"]}]})
    expect(s3.head_object(Bucket=RAW, Key=k)["ContentLength"] == 5 * MIB, "completed object has the wrong size")


@check("anonymous access: only media-public is readable; nothing is writable or listable")
def t_anonymous():
    s3.put_object(Bucket=PUB, Key=key("poster.txt"), Body=b"public", ContentType="text/plain")
    s3.put_object(Bucket=HLS, Key=key("private.txt"), Body=b"private")
    s3.put_object(Bucket=RAW, Key=key("private.txt"), Body=b"private")
    u = lambda b, n: f"{ENDPOINT}/{b}/{key(n)}"  # noqa: E731
    want = [
        ("GET public object", requests.get(u(PUB, "poster.txt"), timeout=TIMEOUT).status_code, 200),
        ("GET hls object", requests.get(u(HLS, "private.txt"), timeout=TIMEOUT).status_code, 403),
        ("GET raw object", requests.get(u(RAW, "private.txt"), timeout=TIMEOUT).status_code, 403),
        ("GET missing hls object", requests.get(u(HLS, "nope.m3u8"), timeout=TIMEOUT).status_code, 403),
        ("PUT public", requests.put(u(PUB, "evil.txt"), data=b"x", timeout=TIMEOUT).status_code, 403),
        ("PUT hls", requests.put(u(HLS, "evil.txt"), data=b"x", timeout=TIMEOUT).status_code, 403),
        ("DELETE public", requests.delete(u(PUB, "poster.txt"), timeout=TIMEOUT).status_code, 403),
        ("LIST public", requests.get(f"{ENDPOINT}/{PUB}?list-type=2", timeout=TIMEOUT).status_code, 403),
    ]
    bad = [f"{n}: got {got}, want {w}" for n, got, w in want if got != w]
    expect(not bad, "; ".join(bad))


@check("range requests: 206 + Content-Range, anonymous (public) and presigned (private)")
def t_range():
    body = bytes(range(256)) * 40
    s3.put_object(Bucket=PUB, Key=key("range.bin"), Body=body, ContentType="application/octet-stream")
    r = requests.get(f"{ENDPOINT}/{PUB}/{key('range.bin')}", headers={"Range": "bytes=10-19"}, timeout=TIMEOUT)
    expect(r.status_code == 206 and r.content == body[10:20], f"public range -> {r.status_code}")
    expect(r.headers.get("Content-Range", "").startswith("bytes 10-19/"), f"Content-Range {r.headers.get('Content-Range')!r}")
    expect(r.headers.get("Accept-Ranges") == "bytes", "Accept-Ranges header missing")
    s3.put_object(Bucket=HLS, Key=key("range.bin"), Body=body)
    url = s3.generate_presigned_url("get_object", Params={"Bucket": HLS, "Key": key("range.bin")}, ExpiresIn=300)
    r = requests.get(url, headers={"Range": "bytes=100-109"}, timeout=TIMEOUT)
    expect(r.status_code == 206 and r.content == body[100:110], f"presigned range -> {r.status_code}")


@check("CORS: allowed origins pass the preflight and can read ETag; other origins are refused")
def t_cors():
    expect(ORIGINS, "S3_ALLOWED_ORIGINS is empty, nothing to test")
    k = key("cors.bin")
    put = s3.generate_presigned_url("put_object", Params={"Bucket": RAW, "Key": k, "ContentType": "video/mp4"}, ExpiresIn=300)
    for origin in ORIGINS:
        pre = requests.options(put, timeout=TIMEOUT, headers={
            "Origin": origin, "Access-Control-Request-Method": "PUT",
            "Access-Control-Request-Headers": "content-type"})
        expect(pre.status_code in (200, 204), f"{origin}: preflight -> {pre.status_code}")
        expect(acao(pre) in (origin, "*"), f"{origin}: Access-Control-Allow-Origin={acao(pre)!r}")
        expect("PUT" in pre.headers.get("Access-Control-Allow-Methods", ""), f"{origin}: PUT not allowed: {pre.headers.get('Access-Control-Allow-Methods')!r}")
        r = requests.put(put, data=b"cors", headers={"Content-Type": "video/mp4", "Origin": origin}, timeout=TIMEOUT)
        expect(r.status_code == 200, f"{origin}: PUT -> {r.status_code}")
        expose = r.headers.get("Access-Control-Expose-Headers", "").lower()
        expect("etag" in expose, f"{origin}: ETag is not exposed to the browser (Expose-Headers={expose!r}). Multipart uploads need it")
        expect(r.headers.get("ETag"), f"{origin}: no ETag header on PUT response")
    pre = requests.options(put, timeout=TIMEOUT, headers={"Origin": BAD_ORIGIN, "Access-Control-Request-Method": "PUT"})
    expect(acao(pre) not in (BAD_ORIGIN, "*"), f"disallowed origin got Access-Control-Allow-Origin={acao(pre)!r}")


@check("listing: prefix + pagination (continuation tokens) and batch delete")
def t_listing():
    names = [key(f"list/{i:03d}.ts") for i in range(25)]
    for n in names:
        s3.put_object(Bucket=HLS, Key=n, Body=b"x")
    seen, token, pages = [], None, 0
    while True:
        kw = {"Bucket": HLS, "Prefix": key("list/"), "MaxKeys": 10}
        if token:
            kw["ContinuationToken"] = token
        page = s3.list_objects_v2(**kw)
        pages += 1
        seen += [o["Key"] for o in page.get("Contents", [])]
        if not page.get("IsTruncated"):
            break
        token = page["NextContinuationToken"]
    expect(sorted(seen) == sorted(names), f"listed {len(seen)} of {len(names)} keys over {pages} pages")
    expect(pages >= 3, f"expected at least 3 pages with MaxKeys=10, got {pages}")
    out = s3.delete_objects(Bucket=HLS, Delete={"Objects": [{"Key": n} for n in names]})
    expect(len(out.get("Deleted", [])) == len(names) and not out.get("Errors"), f"batch delete: {out.get('Errors')}")
    left = s3.list_objects_v2(Bucket=HLS, Prefix=key("list/")).get("KeyCount", 0)
    expect(left == 0, f"{left} keys remain after batch delete")


@check("SDK default checksums (newer AWS SDKs send CRC32 trailers) are accepted")
def t_checksums():
    c = make_client(ENDPOINT, request_checksum_calculation="when_supported", response_checksum_validation="when_supported")
    c.put_object(Bucket=RAW, Key=key("checksum.bin"), Body=os.urandom(4096))
    expect(c.get_object(Bucket=RAW, Key=key("checksum.bin"))["Body"].read(), "empty body")


# ---------------------------------------------------------------- cleanup and entry points
def cleanup():
    for b in (RAW, HLS, PUB):
        try:
            for u in s3.list_multipart_uploads(Bucket=b, Prefix=RUN).get("Uploads", []):
                s3.abort_multipart_upload(Bucket=b, Key=u["Key"], UploadId=u["UploadId"])
            token = None
            while True:
                kw = {"Bucket": b, "Prefix": RUN}
                if token:
                    kw["ContinuationToken"] = token
                page = s3.list_objects_v2(**kw)
                objs = [{"Key": o["Key"]} for o in page.get("Contents", [])]
                if objs:
                    s3.delete_objects(Bucket=b, Delete={"Objects": objs})
                if not page.get("IsTruncated"):
                    break
                token = page["NextContinuationToken"]
        except Exception as e:  # noqa: BLE001
            print(f"warn  cleanup of {b} failed: {e}", file=sys.stderr)


def cmd_check():
    print(f"S3 endpoint {ENDPOINT} | buckets {RAW}, {HLS}, {PUB} | test prefix {RUN}\n", flush=True)
    try:
        for t in (t_buckets, t_bad_creds, t_metadata, t_multipart, t_abort, t_sweep, t_presigned,
                  t_presigned_part, t_anonymous, t_range, t_cors, t_listing, t_checksums):
            t()
    finally:
        cleanup()
    failed = [r for r in results if not r[0]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    if failed:
        print("S3 COMPATIBILITY CHECK FAILED")
        return 1
    print("ALL GOOD")
    return 0


def cmd_seed():
    s3.put_object(Bucket=PUB, Key="_smoke/test.txt", Body=b"streamafrica-range-test-0123456789", ContentType="text/plain")
    print("seeded", f"{PUB}/_smoke/test.txt")
    return 0


def cmd_presign():
    """URLs signed for the PUBLIC endpoint (what a browser would use). The host-side script
    sends them with curl; signing is pure computation, so this container need not reach that host."""
    pub = make_client(PUBLIC_ENDPOINT)
    k = RUN + "browser.bin"
    # A fixed, well-known key is used so the host script can delete it afterwards via `cleanup`.
    put = pub.generate_presigned_url("put_object", Params={"Bucket": RAW, "Key": k, "ContentType": "video/mp4"}, ExpiresIn=300)
    get = pub.generate_presigned_url("get_object", Params={"Bucket": RAW, "Key": k}, ExpiresIn=300)
    print("PUT_URL=" + put)
    print("GET_URL=" + get)
    print("KEY=" + k)
    return 0


def cmd_cleanup():
    prefix = os.environ.get("CLEAN_PREFIX", "_s3check/")
    global RUN
    RUN = prefix
    cleanup()
    return 0


if __name__ == "__main__":
    sub = sys.argv[1] if len(sys.argv) > 1 else "check"
    handlers = {"check": cmd_check, "seed": cmd_seed, "presign": cmd_presign, "cleanup": cmd_cleanup}
    if sub not in handlers:
        print(f"usage: s3_compat.py [{'|'.join(handlers)}]", file=sys.stderr)
        sys.exit(2)
    sys.exit(handlers[sub]())
