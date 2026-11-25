#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把内网 gpuworks-runner 镜像发布到公网 registry，尽量不传 base 的层。

我们的镜像 = 上游 base 镜像的全部层 + 几百 MB 的薄层。目标 registry 若支持跨仓库
blob mount（POST /v2/<目标>/blobs/uploads/?mount=<digest>&from=<源仓库> 返回 201），
base 层就是零传输的“链接”，只需上传薄层。流程：

  1. 读源 index，列出每个平台 manifest 的 config + layers；
  2. 逐层判定：目标仓库已有 -> 跳过；在 base 里且 base 与目标同一 registry -> 尝试
     mount；其余 -> 需上传；
  3. 真实运行：先 mount，再用 skopeo copy --all --preserve-digests 上传剩余层
     （skopeo 会 HEAD 目标已存在的 blob 并跳过，所以此时只传薄层）；
  4. 校验：目标 tag 的 index digest 与源一致，所有 manifest/blob 都在。

仅标准库，Python 3.6 兼容（gpu3 的 python3 是 3.6）。凭据只在进程内读取：
  * --env-file 指向 KEY=VALUE 文件（DOCKERHUB_/QUAY_/SWR_ 的 USERNAME/TOKEN|PASSWORD），
    传 "-" 则从 stdin 读（可 `ssh host python3 publish.py ... --env-file - < registries.env`，
    凭据不落盘）；
  * 凭据不进命令行参数、不进日志（所有输出过一遍脱敏）；
  * 给 skopeo 的凭据走 0600 的临时 authfile（放在 0700 的临时目录），退出时删除。

退出码：
  0 成功（dry-run 为报告完成）   2 参数/配置错误   3 源镜像读取失败
  4 目标认证失败               5 skopeo 重试后仍失败   6 发布后校验失败

用法示例：
  publish.py cuda13.0-sglang0.5.21-r2 dockerhub --dry-run
  publish.py cuda13.0-sglang0.5.21-r2 dockerhub --dest-tag zz-pilot-cuda13.0-sglang0.5.21-r2 \\
      --env-file /path/registries.env
"""
from __future__ import print_function

import argparse
import base64
import concurrent.futures
import hashlib
import http.client as httpclient
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time

try:  # py3
    import urllib.request as urlreq
    import urllib.error as urlerr
    import urllib.parse as urlparse
except ImportError:  # pragma: no cover
    print("python3 required", file=sys.stderr)
    sys.exit(2)

EXIT_OK, EXIT_USAGE, EXIT_SRC, EXIT_AUTH, EXIT_SKOPEO, EXIT_VERIFY = 0, 2, 3, 4, 5, 6

DEFAULT_SRC = "registry.zstack.ai/gpuworks/gpuworks-runner"
DEFAULT_ENV_FILE = "/home/weiw/workspace_vm/gpuworks/secrets/registries.env"
DEFAULT_SKOPEO = "/usr/local/bin/skopeo"
DEFAULT_WORKDIR = "/var/tmp/s7-publish"

MT_INDEX = ("application/vnd.oci.image.index.v1+json",
            "application/vnd.docker.distribution.manifest.list.v2+json")
MT_MANIFEST = ("application/vnd.oci.image.manifest.v1+json",
               "application/vnd.docker.distribution.manifest.v2+json")
ACCEPT_ALL = ", ".join(MT_INDEX + MT_MANIFEST)

# 目标 registry 描述。mount 字段是实测结论（见 README 的“发布”一节），仅影响
# dry-run 的“可 mount”估算与是否尝试 mount；真实运行永远以服务器返回码为准
# （202 -> 回退为上传）。--mount=auto|yes|no 可覆盖。
TARGETS = {
    "dockerhub": {
        "env": "DOCKERHUB", "ref_host": "docker.io", "api_host": "registry-1.docker.io",
        "repo": "gpuworks/gpuworks-runner", "secret_keys": ("DOCKERHUB_TOKEN", "DOCKERHUB_PASSWORD"),
        "mount": True,
    },
    "quay": {
        "env": "QUAY", "ref_host": "quay.io", "api_host": "quay.io",
        "repo": "gpuworks/gpuworks-runner", "secret_keys": ("QUAY_PASSWORD", "QUAY_TOKEN"),
        "mount": True,
    },
    "swr": {
        "env": "SWR", "ref_host": "swr.cn-southwest-2.myhuaweicloud.com",
        "api_host": "swr.cn-southwest-2.myhuaweicloud.com",
        "repo": "gpuworks/gpuworks-runner", "secret_keys": ("SWR_PASSWORD", "SWR_TOKEN"),
        # SWR：mount 只对 SWR 内已有的 blob 有效（base 在 Docker Hub/quay，不可 mount）；
        # blob 的 HEAD 是账号全局的（已推过的层自动判定为已有）；国内链路，必须直连不走代理；
        # 会把 OCI index 改写成 Docker manifest list（index digest 变，平台 manifest digest 不变）。
        "mount": False, "direct": True, "index_rewrite": True,
    },
}


class PublishError(Exception):
    def __init__(self, msg, code=1):
        Exception.__init__(self, msg)
        self.code = code


# --------------------------------------------------------------------------
# 日志与脱敏
# --------------------------------------------------------------------------
_SECRETS = set()
_LOCK = threading.Lock()


def register_secret(value):
    if value and len(value) >= 4:
        _SECRETS.add(value)
        _SECRETS.add(base64.b64encode(value.encode()).decode())


def redact(text):
    for s in sorted(_SECRETS, key=len, reverse=True):
        text = text.replace(s, "***")
    return text


def log(msg):
    line = "[%s] %s" % (time.strftime("%H:%M:%S"), redact(str(msg)))
    with _LOCK:
        print(line, flush=True)


def human(n):
    return "%.1f MiB" % (n / 1048576.0)


# --------------------------------------------------------------------------
# registry 客户端（Bearer/Basic 令牌流，不跟随带凭据的跨主机重定向）
# --------------------------------------------------------------------------
class _NoRedirect(urlreq.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urlreq.build_opener(_NoRedirect)


def force_direct(hosts):
    """把 hosts 加进 no_proxy 并重建 opener（urllib 在构建 opener 时读环境变量）；
    skopeo 子进程继承同一环境。"""
    global _OPENER
    cur = [x for x in os.environ.get("no_proxy", os.environ.get("NO_PROXY", "")).split(",") if x]
    for h in hosts:
        if h not in cur:
            cur.append(h)
    os.environ["no_proxy"] = os.environ["NO_PROXY"] = ",".join(cur)
    _OPENER = urlreq.build_opener(_NoRedirect)


def http(method, url, headers=None, data=None, timeout=60, retries=3):
    """返回 (status, headers(dict lower-case), body bytes)。4xx/5xx 不抛异常。"""
    last = None
    for attempt in range(retries):
        req = urlreq.Request(url, data=data, method=method)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            resp = _OPENER.open(req, timeout=timeout)
            body = resp.read() if method != "HEAD" else b""
            return resp.status, dict((k.lower(), v) for k, v in resp.headers.items()), body
        except urlerr.HTTPError as e:
            body = b""
            try:
                body = e.read() if method != "HEAD" else b""
            except Exception:
                pass
            hdrs = dict((k.lower(), v) for k, v in e.headers.items())
            if e.code >= 500 and attempt + 1 < retries:
                last = "HTTP %d" % e.code
                time.sleep(1 + attempt * 2)
                continue
            return e.code, hdrs, body
        except (urlerr.URLError, OSError, httpclient.HTTPException) as e:
            last = e.__class__.__name__
            if attempt + 1 < retries:
                time.sleep(1 + attempt * 2)
                continue
    raise PublishError("network error on %s %s: %s" % (method, redact(url), last), EXIT_SRC)


def parse_challenge(value):
    scheme = value.split(" ", 1)[0].lower()
    params = dict(re.findall(r'(\w+)="([^"]*)"', value))
    return scheme, params


class Registry(object):
    """一个 registry 主机。creds=(user, secret) 只会发给令牌端点（或 Basic 主机）。"""

    def __init__(self, host, creds=None):
        self.host = host
        self.creds = creds
        self._challenge = None
        self._tokens = {}
        self._tlock = threading.Lock()

    def _basic(self):
        raw = ("%s:%s" % self.creds).encode()
        return "Basic " + base64.b64encode(raw).decode()

    def _discover(self):
        if self._challenge is None:
            st, h, _ = http("GET", "https://%s/v2/" % self.host)
            if st == 401 and "www-authenticate" in h:
                self._challenge = parse_challenge(h["www-authenticate"])
            else:
                self._challenge = ("none", {})
        return self._challenge

    def auth_header(self, scopes, force=False):
        scheme, params = self._discover()
        if scheme == "none":
            return {}
        if scheme == "basic":
            return {"Authorization": self._basic()} if self.creds else {}
        key = tuple(sorted(scopes))
        with self._tlock:
            if not force and key in self._tokens:
                return {"Authorization": "Bearer " + self._tokens[key]}
            q = []
            if params.get("service"):
                q.append(("service", params["service"]))
            for s in key:
                q.append(("scope", s))
            url = params["realm"] + ("&" if "?" in params["realm"] else "?") + urlparse.urlencode(q)
            hdrs = {}
            if self.creds:
                hdrs["Authorization"] = self._basic()
            st, _, body = http("GET", url, headers=hdrs)
            if st != 200:
                raise PublishError("token request to %s failed: HTTP %d (%s)" % (
                    self.host, st, "with credentials" if self.creds else "anonymous"),
                    EXIT_AUTH if self.creds else EXIT_SRC)
            doc = json.loads(body.decode())
            tok = doc.get("token") or doc.get("access_token")
            if not tok:
                raise PublishError("token response from %s has no token" % self.host, EXIT_AUTH)
            register_secret(tok)
            self._tokens[key] = tok
            return {"Authorization": "Bearer " + tok}

    def request(self, method, path, scopes, headers=None, data=None, url=None):
        """带认证请求；401 时刷新一次令牌。手动处理（不带凭据的）重定向。"""
        full = url or ("https://%s%s" % (self.host, path))
        for force in (False, True):
            h = dict(headers or {})
            h.update(self.auth_header(scopes, force=force))
            st, rh, body = http(method, full, headers=h, data=data)
            if st == 401 and not force:
                continue
            break
        hops = 0
        while st in (301, 302, 303, 307, 308) and "location" in rh and hops < 3 \
                and method in ("GET", "HEAD"):
            full = urlparse.urljoin(full, rh["location"])
            h = dict((k, v) for k, v in (headers or {}).items())
            if urlparse.urlparse(full).netloc == self.host:
                h.update(self.auth_header(scopes))
            st, rh, body = http(method, full, headers=h)
            hops += 1
        return st, rh, body

    # ---- 高层 API ----
    def get_manifest(self, repo, ref, accept=ACCEPT_ALL):
        st, h, body = self.request("GET", "/v2/%s/manifests/%s" % (repo, ref),
                                   ["repository:%s:pull" % repo], {"Accept": accept})
        if st != 200:
            return None
        digest = h.get("docker-content-digest") or "sha256:" + hashlib.sha256(body).hexdigest()
        return digest, body, h.get("content-type", "")

    def blob_size(self, repo, digest):
        st, h, _ = self.request("HEAD", "/v2/%s/blobs/%s" % (repo, digest),
                                ["repository:%s:pull" % repo])
        if st == 200:
            return int(h.get("content-length", "0"))
        return None

    def mount(self, repo, digest, from_repo):
        """返回 ('mounted'|'declined'|'error', 说明)。202 表示不支持/不允许，立刻取消会话。"""
        scopes = ["repository:%s:pull,push" % repo, "repository:%s:pull" % from_repo]
        q = urlparse.urlencode([("mount", digest), ("from", from_repo)])
        st, h, _ = self.request("POST", "/v2/%s/blobs/uploads/?%s" % (repo, q), scopes,
                                {"Content-Length": "0"}, data=b"")
        if st == 201:
            return "mounted", "201"
        if st == 202:
            loc = h.get("location")
            if loc:
                loc = urlparse.urljoin("https://%s/" % self.host, loc)
                try:
                    self.request("DELETE", None, scopes, url=loc)
                except PublishError:
                    pass
            return "declined", "202 (mount not honored, upload session cancelled)"
        return "error", "HTTP %d" % st


# --------------------------------------------------------------------------
# 引用解析、matrix、凭据
# --------------------------------------------------------------------------
def split_ref(ref):
    """'vllm/vllm-openai:v0.30.0' -> ('docker.io','vllm/vllm-openai','v0.30.0')"""
    ref = re.sub(r"^docker://", "", ref)
    name, tag = ref, "latest"
    if "@" in name:
        name, tag = name.split("@", 1)
    else:
        last = name.rsplit("/", 1)[-1]
        if ":" in last:
            name, tag = name.rsplit(":", 1)
    parts = name.split("/", 1)
    if len(parts) == 2 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, repo = parts
    else:
        host, repo = "docker.io", name
        if "/" not in repo:
            repo = "library/" + repo
    return host, repo, tag


def api_host(host):
    return "registry-1.docker.io" if host in ("docker.io", "index.docker.io") else host


def matrix_entry(path, tag):
    """轻量解析 matrix.yaml（无 PyYAML）：返回 (id, revision, base)。tag 可带 -<revision>。"""
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    items, cur = [], None
    for ln in lines:
        m = re.match(r"^  - id:\s*(\S+)", ln)
        if m:
            cur = {"id": m.group(1)}
            items.append(cur)
            continue
        if cur is not None:
            m = re.match(r"^    (revision|base):\s*(\S+)", ln)
            if m and m.group(1) not in cur:
                cur[m.group(1)] = m.group(2).strip("\"'")
    for it in items:
        rev = it.get("revision", "")
        if tag == it["id"] or (rev and tag == "%s-%s" % (it["id"], rev)):
            return it
    return None


def check_dest_tag(tag, matrix):
    """目标 tag 必须过 runner tag 文法（tag_check.py，文法由 make image-manifest 导出）。

    检查器或文法文件缺失时拒绝发布而不是放行：单独拷贝 publish.py 到发布机时，
    要把 tag_check.py、tag-grammar.txt（以及 matrix.yaml）一起拷过去。"""
    try:
        import tag_check
        problem = tag_check.refusal(tag, matrix=matrix)
    except (ImportError, IOError, ValueError) as e:
        raise PublishError("cannot check the destination tag (copy tag_check.py, tag-grammar.txt and "
                           "matrix.yaml next to publish.py): %s" % e, EXIT_USAGE)
    if problem:
        raise PublishError(problem, EXIT_USAGE)


def read_env(path):
    out = {}
    stream = sys.stdin if path == "-" else open(path, encoding="utf-8")
    try:
        for ln in stream:
            ln = ln.strip()
            if not ln or ln.startswith("#"):
                continue
            ln = re.sub(r"^export\s+", "", ln)
            if "=" not in ln:
                continue
            k, v = ln.split("=", 1)
            v = v.strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
                v = v[1:-1]
            out[k.strip()] = v
    finally:
        if stream is not sys.stdin:
            stream.close()
    return out


def target_creds(tgt, env, required):
    user = env.get(tgt["env"] + "_USERNAME")
    secret = None
    for k in tgt["secret_keys"]:
        if env.get(k):
            secret = env[k]
            break
    if user:
        register_secret(user)
    if secret:
        register_secret(secret)
    if user and secret:
        return (user, secret)
    if required:
        raise PublishError("no credentials for %s in env file (need %s_USERNAME and one of %s)" % (
            tgt["env"], tgt["env"], "/".join(tgt["secret_keys"])), EXIT_USAGE)
    return None


# --------------------------------------------------------------------------
# 镜像结构
# --------------------------------------------------------------------------
def platform_key(p):
    return "%s/%s%s" % (p.get("os"), p.get("architecture"), "/" + p["variant"] if p.get("variant") else "")


def load_platforms(reg, repo, ref):
    """返回 (index_digest, index_bytes, {platform: (manifest_digest, manifest_json)})。
    源若是单 manifest 则平台为 'single'。"""
    got = reg.get_manifest(repo, ref)
    if not got:
        raise PublishError("cannot read %s/%s:%s" % (reg.host, repo, ref), EXIT_SRC)
    digest, body, _ = got
    doc = json.loads(body.decode())
    if doc.get("mediaType") in MT_MANIFEST or "layers" in doc:
        return digest, body, {"single": (digest, doc)}
    plats = {}
    for m in doc.get("manifests", []):
        ann = m.get("annotations") or {}
        p = m.get("platform") or {}
        if ann.get("vnd.docker.reference.type") == "attestation-manifest" or p.get("os") == "unknown":
            continue
        sub = reg.get_manifest(repo, m["digest"], accept=", ".join(MT_MANIFEST))
        if not sub:
            raise PublishError("cannot read platform manifest %s of %s" % (m["digest"], ref), EXIT_SRC)
        plats[platform_key(p)] = (m["digest"], json.loads(sub[1].decode()))
    if not plats:
        raise PublishError("no runnable platform in %s" % ref, EXIT_SRC)
    return digest, body, plats


def blobs_of(manifest):
    """[(digest, size, kind)]，含 config。"""
    out = []
    cfg = manifest.get("config")
    if cfg:
        out.append((cfg["digest"], cfg["size"], "config"))
    for l in manifest.get("layers", []):
        out.append((l["digest"], l["size"], "layer"))
    return out


def base_layer_sets(base_ref, plats):
    """base 镜像各平台的层摘要集合；base 读不到（私有/内网/不存在）返回 {}。"""
    host, repo, tag = split_ref(base_ref)
    reg = Registry(api_host(host))
    try:
        _, _, bplats = load_platforms(reg, repo, tag)
    except PublishError as e:
        log("base %s 不可读，视为无 mount 候选: %s" % (base_ref, e))
        return host, repo, {}
    sets = {}
    for pk in plats:
        # 平台名在两边一致（linux/amd64、linux/arm64）；base 可能多出 variant，容忍前缀
        bm = bplats.get(pk)
        if bm is None:
            for k, v in bplats.items():
                if k.startswith(pk):
                    bm = v
                    break
        if bm is not None:
            sets[pk] = set(d for d, _, _ in blobs_of(bm[1]))
    return host, repo, sets


# --------------------------------------------------------------------------
# 计划
# --------------------------------------------------------------------------
class Plan(object):
    def __init__(self):
        self.blobs = {}      # digest -> {size, kind, platforms:set}
        self.present = {}    # digest -> size
        self.mountable = {}  # digest -> from_repo
        self.upload = {}     # digest -> size
        self.unchecked = 0   # 目标仓库不可读时，无法判断“已存在”的层数


def build_plan(src_reg, src_repo, src_tag, dst_reg, dst_repo, base_info, mount_ok):
    sidx_digest, sidx_body, plats = load_platforms(src_reg, src_repo, src_tag)
    base_host, base_repo, base_sets = base_info
    plan = Plan()
    plan.src_digest = sidx_digest
    plan.plats = plats
    same_registry = mount_ok and api_host(base_host) == dst_reg.host
    for pk, (_, man) in sorted(plats.items()):
        for d, size, kind in blobs_of(man):
            e = plan.blobs.setdefault(d, {"size": size, "kind": kind, "platforms": set()})
            e["platforms"].add(pk)
    # 同一 digest 若在任一平台的 base 里 -> 可 mount 候选
    base_all = set()
    for s in base_sets.values():
        base_all |= s
    digests = sorted(plan.blobs)
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
        sizes = dict(zip(digests, ex.map(lambda x: dst_reg.blob_size(dst_repo, x), digests)))
    for d in digests:
        e = plan.blobs[d]
        have = sizes[d]
        if have is not None:
            plan.present[d] = e["size"]
        elif same_registry and d in base_all:
            plan.mountable[d] = base_repo
        else:
            plan.upload[d] = e["size"]
    return plan


def report(plan, tag_label, mount_note):
    tot = len(plan.blobs)
    mb = sum(plan.blobs[d]["size"] for d in plan.mountable)
    ub = sum(plan.upload.values())
    log("%s: %d 个 blob（含 config）：目标已有 %d，可 mount %d（%s），需上传 %d" % (
        tag_label, tot, len(plan.present), len(plan.mountable), human(mb), len(plan.upload)))
    log("  mount 说明: %s" % mount_note)
    log("  预计上传 %s（压缩后字节；若 mount 被拒绝则回退为上传，上限 %s）" % (
        human(ub), human(ub + mb)))
    for pk in sorted(set(p for e in plan.blobs.values() for p in e["platforms"])):
        ds = [d for d, e in plan.blobs.items() if pk in e["platforms"]]
        u = [d for d in ds if d in plan.upload]
        log("  %-14s %3d blob，需上传 %2d 个 / %s" % (
            pk, len(ds), len(u), human(sum(plan.upload[d] for d in u))))
    for d in sorted(plan.upload, key=lambda x: -plan.upload[x])[:5]:
        log("    最大上传层 %s %s [%s]" % (d[7:19], human(plan.upload[d]), plan.blobs[d]["kind"]))


# --------------------------------------------------------------------------
# 执行：mount -> skopeo -> verify
# --------------------------------------------------------------------------
def do_mounts(dst_reg, dst_repo, plan, workers):
    """返回 (mounted, declined_or_error)。同一源仓库连续被拒绝 3 次即不再尝试。"""
    mounted, failed = [], []
    stop = threading.Event()
    declined = [0]
    dlock = threading.Lock()

    def one(d):
        if stop.is_set():
            return d, "skipped", "mount disabled after repeated declines"
        status, note = dst_reg.mount(dst_repo, d, plan.mountable[d])
        with dlock:
            if status == "declined":
                declined[0] += 1
                if declined[0] >= 3 and not mounted:
                    stop.set()
            elif status == "mounted":
                declined[0] = 0
        return d, status, note

    items = sorted(plan.mountable, key=lambda x: -plan.blobs[x]["size"])
    done = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        for d, status, note in ex.map(one, items):
            done += 1
            size = plan.blobs[d]["size"]
            if status == "mounted":
                mounted.append(d)
            else:
                failed.append(d)
            log("  mount [%d/%d] %s %-9s %s %s" % (done, len(items), d[7:19], status, human(size), note))
    return mounted, failed


def run_skopeo(skopeo, src_ref, dst_ref, authfile, retries, plan, preserve_digests=True):
    """skopeo copy --all --preserve-digests；返回 (rc, 实际上传的 blob 字节数估计)。"""
    short = dict((d[7:19], plan.blobs[d]["size"]) for d in plan.blobs)
    env = dict(os.environ)
    env.pop("REGISTRY_AUTH_FILE", None)
    # SWR 只收 Docker manifest list：--preserve-digests 会因“不能改写 index”而在最后一步失败
    # （blob 与平台 manifest 都已上传）。去掉它后 skopeo 只转换 index，平台 manifest 原样写入
    # （digest 不变，发布后由 verify 逐平台核对）。
    cmd = [skopeo, "copy", "--all"] + (["--preserve-digests"] if preserve_digests else []) + [
           "--retry-times", "3", "--src-no-creds", "--dest-authfile", authfile,
           "docker://" + src_ref, "docker://" + dst_ref]
    sent = 0
    rc = 1
    for attempt in range(1, retries + 1):
        log("skopeo 第 %d/%d 次: %s -> %s" % (attempt, retries, " ".join(cmd[1:4]), dst_ref))
        t0 = time.time()
        p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             universal_newlines=True, env=env)
        sent = 0
        for ln in p.stdout:
            ln = ln.rstrip()
            if not ln:
                continue
            m = re.match(r"Copying blob (?:sha256:)?([0-9a-f]{12,})\s+(done|skipped)", ln)
            if m:
                key = m.group(1)[:12]
                if m.group(2) == "done":
                    sent += short.get(key, 0)
                    log("  skopeo 已上传 %s %s" % (key, human(short.get(key, 0))))
                else:
                    log("  skopeo 跳过(目标已有) %s" % key)
            elif not re.match(r"Copying blob", ln):
                log("  skopeo: " + ln[:300])
        rc = p.wait()
        log("skopeo rc=%d 用时 %.0fs，本次上传 blob 合计 %s" % (rc, time.time() - t0, human(sent)))
        if rc == 0:
            return 0, sent
        if attempt < retries:
            wait = min(60, 5 * attempt * attempt)
            log("失败，%ds 后重试（目标已完整上传的 blob 会被 skopeo 跳过，不重传）" % wait)
            time.sleep(wait)
    return rc, sent


def verify(src_reg, src_repo, src_tag, dst_reg, dst_repo, dst_tag, src_digest, plats, index_rewrite=False):
    problems = []
    got = dst_reg.get_manifest(dst_repo, dst_tag)
    if not got:
        return ["目标 tag %s 读取失败" % dst_tag]
    if got[0] != src_digest and index_rewrite:
        log("校验: 目标 registry 改写了 index（源 %s -> 目标 %s），改以各平台 manifest digest 逐一核对" % (
            src_digest[7:19], got[0][7:19]))
    elif got[0] != src_digest:
        problems.append("index digest 不一致：源 %s 目标 %s" % (src_digest, got[0]))
    else:
        log("校验: index digest 一致 %s" % got[0])
    doc = json.loads(got[1].decode())
    dplat = [platform_key(m.get("platform") or {}) for m in doc.get("manifests", [])
             if (m.get("platform") or {}).get("os") != "unknown"]
    for pk in plats:
        if pk != "single" and pk not in dplat:
            problems.append("目标缺平台 " + pk)
    for pk, (md, man) in plats.items():
        sub = dst_reg.get_manifest(dst_repo, md, accept=", ".join(MT_MANIFEST))
        if not sub or sub[0] != md:
            problems.append("平台 %s manifest %s 缺失/不一致" % (pk, md[:19]))
            continue
        ds = [d for d, _, _ in blobs_of(man)]
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
            miss = [d for d, sz in zip(ds, ex.map(lambda x: dst_reg.blob_size(dst_repo, x), ds)) if sz is None]
        if miss:
            problems.append("平台 %s 缺 %d 个 blob（如 %s）" % (pk, len(miss), miss[0][:19]))
        else:
            log("校验: %s manifest %s 与 %d 个 blob 均在目标" % (pk, md[7:19], len(blobs_of(man))))
    return problems


def write_authfile(dirpath, tgt, creds):
    path = os.path.join(dirpath, "auth.json")
    blob = {"auths": {tgt["ref_host"]: {"auth": base64.b64encode(("%s:%s" % creds).encode()).decode()}}}
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(blob, f)
    return path


# --------------------------------------------------------------------------
def main(argv=None):
    ap = argparse.ArgumentParser(description="发布 gpuworks-runner 镜像到公网 registry（尽量 mount base 层）")
    ap.add_argument("tag", help="源 tag，如 cuda13.0-sglang0.5.21-r2")
    ap.add_argument("target", choices=sorted(TARGETS), help="目标 registry")
    ap.add_argument("--dest-tag", help="目标 tag（默认同源 tag）")
    ap.add_argument("--src", default=DEFAULT_SRC, help="源仓库（默认 %(default)s）")
    ap.add_argument("--dest-repo", help="覆盖目标仓库路径（默认 gpuworks/gpuworks-runner）")
    ap.add_argument("--base", help="覆盖 matrix.yaml 里的 base 引用（用于推 mount 候选）")
    ap.add_argument("--matrix", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "matrix.yaml"))
    ap.add_argument("--env-file", default=DEFAULT_ENV_FILE, help="凭据文件，'-' 读 stdin（默认 %(default)s）")
    ap.add_argument("--dry-run", action="store_true", help="只报告，不做任何写入（不 mount、不上传）")
    ap.add_argument("--mount", choices=("auto", "yes", "no"), default="auto",
                    help="auto=按 TARGETS 的实测结论；yes=强制尝试；no=不 mount")
    ap.add_argument("--mount-only", action="store_true", help="只做 mount，不跑 skopeo")
    ap.add_argument("--mount-workers", type=int, default=6)
    ap.add_argument("--retries", type=int, default=5, help="skopeo 失败重试次数（默认 %(default)s）")
    ap.add_argument("--skopeo", default=DEFAULT_SKOPEO)
    ap.add_argument("--workdir", default=DEFAULT_WORKDIR, help="临时 authfile 的父目录")
    ap.add_argument("--no-verify", action="store_true")
    a = ap.parse_args(argv)

    tgt = TARGETS[a.target]
    if tgt.get("direct"):
        force_direct([tgt["api_host"]])
        log("%s 走直连（已加入 no_proxy）" % tgt["api_host"])
    dst_repo = a.dest_repo or tgt["repo"]
    dst_tag = a.dest_tag or a.tag
    tmpdir = None
    try:
        check_dest_tag(dst_tag, a.matrix)
        # 凭据：dry-run 也尝试读（目标仓库可能是私有的，读存在性需要 pull 令牌），读不到就匿名
        env = {}
        try:
            env = read_env(a.env_file)
        except IOError as e:
            if not a.dry_run:
                raise PublishError("cannot read env file: %s" % e.__class__.__name__, EXIT_USAGE)
        creds = target_creds(tgt, env, required=not a.dry_run)
        env = None

        src_host, src_repo, _ = split_ref(a.src + ":x")
        src_reg = Registry(api_host(src_host))
        dst_reg = Registry(tgt["api_host"], creds)

        ent = matrix_entry(a.matrix, a.tag) if os.path.exists(a.matrix) else None
        base_ref = a.base or (ent or {}).get("base")
        if base_ref:
            log("base = %s（matrix: %s）" % (base_ref, ent["id"] if ent else "--base"))
            base_info = base_layer_sets(base_ref, load_platforms(src_reg, src_repo, a.tag)[2])
        else:
            log("警告: matrix.yaml 里找不到 %s 且未给 --base，无 mount 候选" % a.tag)
            base_info = ("", "", {})

        mount_ok = {"auto": tgt["mount"], "yes": True, "no": False}[a.mount]
        base_host = base_info[0]
        if mount_ok and base_host and api_host(base_host) != tgt["api_host"]:
            note = "base 在 %s，目标在 %s，不同 registry，无法 mount" % (base_host, tgt["ref_host"])
        elif not mount_ok:
            note = ("%s 不做跨 registry mount（--mount=%s）；blob 存在性按目标 HEAD 判定"
                    "（SWR 的 HEAD 是账号全局的，已推过的层自动跳过）" % (a.target, a.mount))
        else:
            note = "候选源仓库 %s/%s，同一 registry" % (base_host, base_info[1])

        plan = build_plan(src_reg, src_repo, a.tag, dst_reg, dst_repo, base_info, mount_ok)
        label = "%s:%s -> %s/%s:%s" % (a.src, a.tag, tgt["ref_host"], dst_repo, dst_tag)
        log("计划 " + label + ("  [DRY-RUN，无任何写入]" if a.dry_run else ""))
        report(plan, a.tag, note)
        if a.dry_run:
            return EXIT_OK

        t0 = time.time()
        mounted, mfail = [], []
        if plan.mountable:
            log("开始 mount %d 个 blob ..." % len(plan.mountable))
            mounted, mfail = do_mounts(dst_reg, dst_repo, plan, a.mount_workers)
            log("mount 结果: 成功 %d，回退为上传 %d（%s）" % (
                len(mounted), len(mfail), human(sum(plan.blobs[d]["size"] for d in mfail))))
        if a.mount_only:
            return EXIT_OK

        os.makedirs(a.workdir, exist_ok=True)
        tmpdir = tempfile.mkdtemp(prefix="pub-", dir=a.workdir)
        os.chmod(tmpdir, 0o700)
        authfile = write_authfile(tmpdir, tgt, creds)
        src_ref = "%s/%s:%s" % (src_host, src_repo, a.tag)
        dst_ref = "%s/%s:%s" % (tgt["ref_host"], dst_repo, dst_tag)
        rc, sent = run_skopeo(a.skopeo, src_ref, dst_ref, authfile, a.retries, plan,
                              preserve_digests=not tgt.get("index_rewrite", False))
        if rc != 0:
            raise PublishError("skopeo 重试 %d 次后仍失败 rc=%d" % (a.retries, rc), EXIT_SKOPEO)
        log("上传完成：计划上传 blob %s（skopeo 非 TTY 不逐 blob 输出，此为计划值），mount 链接 %d 个，"
            "总用时 %.0fs" % (human(sum(plan.upload.values())), len(mounted), time.time() - t0))
        if not a.no_verify:
            probs = verify(src_reg, src_repo, a.tag, dst_reg, dst_repo, dst_tag,
                           plan.src_digest, plan.plats, tgt.get("index_rewrite", False))
            if probs:
                for p in probs:
                    log("校验失败: " + p)
                raise PublishError("发布后校验失败", EXIT_VERIFY)
        log("发布成功 " + label)
        return EXIT_OK
    except PublishError as e:
        log("错误: %s" % e)
        return e.code
    finally:
        if tmpdir:
            shutil.rmtree(tmpdir, ignore_errors=True)


def _term(signum, frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, _term)
    sys.exit(main())
