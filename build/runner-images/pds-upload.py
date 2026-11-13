#!/usr/bin/env python3
"""Upload an image archive to the offline-download share on Aliyun Drive (PDS).

The share is the folder that docs/how-to/deploy-offline.md links to; it holds
one directory per engine and framework (vllm-cuda/, sglang-dtk/, ...) with
files named gpuworks-runner_<tag>_linux-<arch>.tar.gz, and gpuworks-agent/
with the Kubernetes agent image, gpuworks-agent_<revision>_linux-<arch>.tar.gz
(build/agent-image). The API key this uses
can upload and rename but cannot delete, so every upload is permanent: the
script refuses to overwrite a file that already exists, and --dry-run checks
the destination without writing anything.

Standard library only and Python 3.6 compatible, so it runs on the build host
as is. Parts go up in parallel (16 streams; a single stream manages under
1 MiB/s from the lab network), which PDS only accepts when the file is created
with parallel_upload and every part carries the SHA-1 state of the bytes before
it (see sha1_part_contexts). Part PUTs must not carry a Content-Type, or the
presigned URL answers 403 (see put_part).

Credentials come from the environment (PDS_DOMAIN_ID, PDS_API_KEY), for example `env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py ...`.

    ./pds-upload.py gpuworks-runner_dtk26.04-vllm0.21.0-r1_linux-amd64.tar.gz vllm-dtk --dry-run
    ./pds-upload.py gpuworks-runner_dtk26.04-vllm0.21.0-r1_linux-amd64.tar.gz vllm-dtk
"""
import argparse
import concurrent.futures
import ctypes
import ctypes.util
import http.client
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

DRIVE_ID = "101"
SHARE_ROOT = "/gpuworks-images"
PART_BYTES = 256 << 20
SEND_CHUNK = 4 << 20
WORKERS = 16
RENEW_MARGIN = 600  # re-sign a part URL this many seconds before it expires
NAME = re.compile(r"^gpuworks-(runner|agent)_[A-Za-z0-9._-]+_linux-(amd64|arm64)\.tar\.gz$")


def api(domain, key, path, body):
    request = urllib.request.Request(
        "https://%s.api.aliyunfile.com%s" % (domain, path),
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            raw = response.read()
    except urllib.error.HTTPError as err:
        raise SystemExit("%s failed: HTTP %d %s" % (path, err.code, err.read().decode(errors="replace")[:300]))
    return json.loads(raw.decode()) if raw else {}


def find(domain, key, folder_id, name):
    # file/list returns no items for this share's folders; search by name does.
    query = 'parent_file_id = "%s" and name = "%s"' % (folder_id, name)
    return api(domain, key, "/v2/file/search", {"drive_id": DRIVE_ID, "query": query, "limit": 10}).get("items", [])


def put_part(url, handle, offset, length):
    """Stream [offset, offset+length) of handle to a presigned part URL.

    The URL is an OSS v1 signature over the request headers, including
    Content-Type. urllib adds `Content-Type: application/x-www-form-urlencoded`
    to every PUT with a body, which makes the signature mismatch (HTTP 403), so
    this sends exactly Host and Content-Length through http.client.
    """
    parts = urllib.parse.urlsplit(url)
    target = parts.path + (("?" + parts.query) if parts.query else "")
    conn_class = http.client.HTTPSConnection if parts.scheme == "https" else http.client.HTTPConnection
    conn = conn_class(parts.hostname, parts.port, timeout=1800)
    try:
        conn.putrequest("PUT", target, skip_accept_encoding=True)
        conn.putheader("Content-Length", str(length))
        conn.endheaders()
        handle.seek(offset)
        remaining = length
        while remaining > 0:
            chunk = handle.read(min(SEND_CHUNK, remaining))
            if not chunk:
                raise IOError("unexpected end of file at offset %d" % handle.tell())
            conn.send(chunk)
            remaining -= len(chunk)
        response = conn.getresponse()
        body = response.read().decode("utf-8", "replace")
        return response.status, body
    finally:
        conn.close()


def url_expiry(url):
    query = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)
    for name in ("Expires", "x-oss-expires"):
        if name in query:
            try:
                return int(query[name][0])
            except ValueError:
                pass
    return None


class SHA_CTX(ctypes.Structure):
    _fields_ = [("h0", ctypes.c_uint32), ("h1", ctypes.c_uint32), ("h2", ctypes.c_uint32),
                ("h3", ctypes.c_uint32), ("h4", ctypes.c_uint32), ("Nl", ctypes.c_uint32),
                ("Nh", ctypes.c_uint32), ("data", ctypes.c_uint32 * 16), ("num", ctypes.c_uint32)]


def sha1_part_contexts(path, part_bytes, parts):
    """SHA-1 state at every part boundary.

    PDS rejects out-of-order part uploads (PartNotSequential) unless the file is
    created with parallel_upload and each part carries the SHA-1 state of all
    bytes before it. hashlib cannot export that state, so this drives OpenSSL's
    SHA1_* through ctypes and reads h0..h4 out of SHA_CTX at each boundary
    (part_bytes is a multiple of 64, so the boundary is a block boundary).
    """
    lib = None
    for name in (ctypes.util.find_library("crypto"), "libcrypto.so.3", "libcrypto.so.1.1", "libcrypto.so.10", "libcrypto.so"):
        if not name:
            continue
        try:
            candidate = ctypes.CDLL(name)
        except OSError:
            continue
        if hasattr(candidate, "SHA1_Init"):
            lib = candidate
            break
    if lib is None:
        raise SystemExit("libcrypto with SHA1_* symbols not found")
    lib.SHA1_Init.argtypes = [ctypes.POINTER(SHA_CTX)]
    lib.SHA1_Update.argtypes = [ctypes.POINTER(SHA_CTX), ctypes.c_char_p, ctypes.c_size_t]
    ctx = SHA_CTX()
    lib.SHA1_Init(ctypes.byref(ctx))
    contexts = {}
    consumed = 0
    with open(path, "rb") as source:
        for number in range(1, parts + 1):
            boundary = (number - 1) * part_bytes
            while consumed < boundary:
                buf = source.read(min(8 << 20, boundary - consumed))
                if not buf:
                    raise SystemExit("short read while hashing %s" % path)
                lib.SHA1_Update(ctypes.byref(ctx), buf, len(buf))
                consumed += len(buf)
            if ctx.num != 0:
                raise SystemExit("part %d boundary is not block aligned" % number)
            contexts[number] = {"part_offset": boundary, "h": [ctx.h0, ctx.h1, ctx.h2, ctx.h3, ctx.h4]}
    return contexts


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("archive", help="gpuworks-runner_<tag>_linux-<arch>.tar.gz or gpuworks-agent_<revision>_linux-<arch>.tar.gz")
    parser.add_argument("folder", help="share subdirectory, e.g. vllm-dtk")
    parser.add_argument("--dry-run", action="store_true", help="check the destination, upload nothing")
    args = parser.parse_args()

    name = os.path.basename(args.archive)
    if not NAME.match(name):
        raise SystemExit("archive name must be gpuworks-runner_<tag>_linux-<arch>.tar.gz or gpuworks-agent_<revision>_linux-<arch>.tar.gz, got %s" % name)
    domain, key = os.environ.get("PDS_DOMAIN_ID"), os.environ.get("PDS_API_KEY")
    if not domain or not key:
        raise SystemExit("PDS_DOMAIN_ID and PDS_API_KEY must be set")

    folder = api(domain, key, "/v2/file/get_by_path", {"drive_id": DRIVE_ID, "file_path": SHARE_ROOT + "/" + args.folder})
    if folder.get("type") != "folder":
        raise SystemExit("%s/%s is not a folder on the share" % (SHARE_ROOT, args.folder))
    if find(domain, key, folder["file_id"], name):
        raise SystemExit("%s/%s/%s already exists; uploads cannot be deleted, so it is left as is" % (SHARE_ROOT, args.folder, name))
    size = os.path.getsize(args.archive)
    parts = (size + PART_BYTES - 1) // PART_BYTES
    print("%s -> %s/%s (%d bytes, %d parts)" % (name, SHARE_ROOT, args.folder, size, parts), flush=True)
    if args.dry_run:
        return

    part_info = [{"part_number": n} for n in range(1, parts + 1)]
    body = {"drive_id": DRIVE_ID, "parent_file_id": folder["file_id"], "name": name, "type": "file",
            "check_name_mode": "refuse", "size": size}
    if parts > 1:
        started = time.time()
        contexts = sha1_part_contexts(args.archive, PART_BYTES, parts)
        print("hashed part boundaries in %.0fs" % (time.time() - started), flush=True)
        for entry in part_info:
            entry["parallel_sha1_ctx"] = contexts[entry["part_number"]]
        body["parallel_upload"] = True
    body["part_info_list"] = part_info
    created = api(domain, key, "/v2/file/create", body)
    file_id, upload_id = created["file_id"], created["upload_id"]
    print("file_id=%s upload_id=%s" % (file_id, upload_id), flush=True)
    urls = {p["part_number"]: p["upload_url"] for p in created["part_info_list"]}
    lock = threading.Lock()
    progress = {"done": 0, "started": time.time()}

    def send_one(number):
        offset = (number - 1) * PART_BYTES
        length = min(PART_BYTES, size - offset)
        with open(args.archive, "rb") as handle:
            for attempt in range(6):
                url = urls.get(number)
                expiry = url_expiry(url) if url else None
                if url is None or (expiry and time.time() > expiry - RENEW_MARGIN):
                    fresh = api(domain, key, "/v2/file/get_upload_url", {
                        "drive_id": DRIVE_ID, "file_id": file_id, "upload_id": upload_id,
                        # A parallel upload must re-send the part's SHA-1 state, or PDS rejects it.
                        "part_info_list": [part_info[number - 1]],
                    })
                    url = urls[number] = fresh["part_info_list"][0]["upload_url"]
                try:
                    status, text = put_part(url, handle, offset, length)
                except (OSError, http.client.HTTPException) as err:
                    if attempt == 5:
                        raise
                    print("  part %d transport error (%s), retry %d" % (number, err, attempt + 1), flush=True)
                    urls.pop(number, None)
                    time.sleep(3 * (attempt + 1))
                    continue
                if status in (200, 201):
                    break
                if attempt < 5 and status in (403, 408, 409, 500, 502, 503, 504):
                    print("  part %d HTTP %d, re-signing: %s" % (number, status, text[:200].replace("\n", " ")), flush=True)
                    urls.pop(number, None)
                    time.sleep(2 * (attempt + 1))
                    continue
                raise RuntimeError("part %d failed: HTTP %d %s" % (number, status, text[:300]))
        with lock:
            progress["done"] += length
            rate = progress["done"] / max(time.time() - progress["started"], 0.001)
            print("  part %d/%d ok (%.2f/%.2f GiB, %.1f MiB/s)" % (
                number, parts, progress["done"] / 2.0 ** 30, size / 2.0 ** 30, rate / 2.0 ** 20), flush=True)

    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS if parts > 1 else 1) as pool:
        futures = [pool.submit(send_one, n) for n in range(1, parts + 1)]
        errors = [f.exception() for f in concurrent.futures.as_completed(futures) if f.exception() is not None]
    if errors:
        raise SystemExit("upload failed: %s (file_id=%s upload_id=%s; nothing was completed)" % (errors[0], file_id, upload_id))
    done = api(domain, key, "/v2/file/complete", {"drive_id": DRIVE_ID, "file_id": file_id, "upload_id": upload_id})
    if done.get("size") != size:
        raise SystemExit("complete reported size %s, expected %d" % (done.get("size"), size))
    print("uploaded %s/%s/%s" % (SHARE_ROOT, args.folder, name))


if __name__ == "__main__":
    sys.exit(main())
