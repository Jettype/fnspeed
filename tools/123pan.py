#!/usr/bin/env python3
"""123 云盘开放平台：换 access_token、取文件直链、产出 fnspeed 云端清单条目。

前提：账号需开通「开发者权益包」，并在控制台创建应用拿到 clientID / clientSecret。
官方文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced

接口（见官方「获取直链链接」「获取 access_token」两篇）：

    POST /api/v1/access_token
        Header: Platform: open_platform
        Body:   {"clientID": "...", "clientSecret": "..."}
        → data.accessToken（JWT，约 90 天）

    GET  /api/v1/direct-link/url?fileID=<id>
        Header: Platform: open_platform, Authorization: Bearer <token>
        → data.url（https://{uid}.cdn.123clouddisk.com/... 直链）

用法：

    # 1. 换 token 并缓存（之后自动复用，快过期会自动续）
    ./tools/123pan.py token --client-id xxx --client-secret yyy

    # 2. 取直链
    ./tools/123pan.py link FILE_ID

    # 3. 只看会生成什么配置集
    ./tools/123pan.py profile FILE_ID --id pan123-big --name "123云盘 · 自有大文件"

    # 4. 直接写进清单（自动改 rev、自动去重）
    ./tools/123pan.py add FILE_ID --id pan123-big -f cloud/sources.json

凭据也可以放在 etc/123pan.json（该目录已在 .gitignore 里）：

    {"clientID": "...", "clientSecret": "..."}

环境变量同样可用：PAN123_CLIENT_ID / PAN123_CLIENT_SECRET / PAN123_TOKEN。
"""

import argparse
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API_BASE = "https://open-api.123pan.com"
TOKEN_PATH = "etc/123pan-token.json"
CRED_PATH = "etc/123pan.json"
UA = "fnspeed-123pan-tool/1.0"


# ---------------------------------------------------------------------------
# 基础
# ---------------------------------------------------------------------------

def die(msg):
    sys.exit("✗ " + msg)


def read_json(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def write_json(path, doc, mode=0o600):
    d = os.path.dirname(os.path.abspath(path))
    if d:
        os.makedirs(d, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="\n") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, path)
    try:
        os.chmod(path, mode)
    except OSError:
        pass


def api(method, path, body=None, token=None, timeout=15):
    """打一次开放平台接口，返回 data 字段。code != 0 一律当失败。"""
    url = API_BASE + path
    data = None
    headers = {"Platform": "open_platform", "Accept": "application/json", "User-Agent": UA}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = "Bearer " + token

    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:300]
        die("HTTP %d：%s" % (e.code, detail))
    except urllib.error.URLError as e:
        die("网络错误：%s" % e.reason)

    try:
        j = json.loads(raw)
    except ValueError:
        die("返回的不是 JSON：%s" % raw[:200])

    if j.get("code") != 0:
        die("接口返回 code=%s message=%s" % (j.get("code"), j.get("message")))
    return j.get("data") or {}


# ---------------------------------------------------------------------------
# 凭据与 token
# ---------------------------------------------------------------------------

def jwt_exp(token):
    """从 JWT 里读出 exp（秒）。解不出来就返回 0。"""
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        doc = json.loads(base64.urlsafe_b64decode(payload))
        return int(doc.get("exp") or 0)
    except Exception:
        return 0


def load_creds(args):
    cid = args.client_id or os.environ.get("PAN123_CLIENT_ID")
    sec = args.client_secret or os.environ.get("PAN123_CLIENT_SECRET")
    if (not cid or not sec) and os.path.exists(CRED_PATH):
        doc = read_json(CRED_PATH)
        cid = cid or doc.get("clientID")
        sec = sec or doc.get("clientSecret")
    if not cid or not sec:
        die(
            "缺少 clientID / clientSecret。三种给法任选一种：\n"
            "  --client-id / --client-secret\n"
            "  环境变量 PAN123_CLIENT_ID / PAN123_CLIENT_SECRET\n"
            "  写进 %s：{\"clientID\":\"...\",\"clientSecret\":\"...\"}\n"
            "（开放平台 → 控制台 → 创建应用后才有；需先开通开发者权益包）" % CRED_PATH
        )
    return cid, sec


def get_token(args, force=False):
    tok = args.token or os.environ.get("PAN123_TOKEN")
    if tok:
        return tok

    if not force and os.path.exists(TOKEN_PATH):
        doc = read_json(TOKEN_PATH)
        cached = doc.get("accessToken") or ""
        exp = int(doc.get("exp") or jwt_exp(cached))
        # 提前 1 天续期，免得跑到一半过期
        if cached and exp and exp - 86400 > time.time():
            return cached

    cid, sec = load_creds(args)
    data = api("POST", "/api/v1/access_token", {"clientID": cid, "clientSecret": sec})
    tok = data.get("accessToken") or ""
    if not tok:
        die("接口没返回 accessToken：%s" % json.dumps(data, ensure_ascii=False))

    exp = jwt_exp(tok)
    write_json(TOKEN_PATH, {"accessToken": tok, "exp": exp, "savedAt": int(time.time())})
    left = (exp - time.time()) / 86400 if exp else 0
    print("已保存 token 到 %s（剩余约 %.0f 天）" % (TOKEN_PATH, left))
    return tok


# ---------------------------------------------------------------------------
# 直链
# ---------------------------------------------------------------------------

def direct_link(token, file_id):
    q = urllib.parse.urlencode({"fileID": int(file_id)})
    data = api("GET", "/api/v1/direct-link/url?" + q, token=token)
    url = (data.get("url") or "").strip()
    if not url:
        die("接口没返回直链：%s" % json.dumps(data, ensure_ascii=False))
    return url


def build_profile(url, pid, name, group, desc, mode):
    return {
        "id": pid,
        "name": name or pid,
        "desc": desc or ("123 云盘直链：" + url.split("?")[0].rsplit("/", 1)[-1]),
        "group": group,
        "mode": mode,
        "downloadUrls": [url] if mode in ("down", "both") else [],
        "uploadUrls": [] if mode in ("down", "both") else [url],
    }


# ---------------------------------------------------------------------------
# 写进清单
# ---------------------------------------------------------------------------

def bump_rev(old, today):
    """rev 用「日期.序号」，同一天多次改动就递增序号。"""
    m = re.match(r"^(\d{4}-\d{2}-\d{2})\.(\d+)$", old or "")
    if m and m.group(1) == today:
        return "%s.%d" % (today, int(m.group(2)) + 1)
    return today + ".1"


def add_to_sources(path, profile):
    doc = read_json(path)
    if doc.get("format") != "FNS_CLOUD_SOURCES":
        die("%s 的 format 不是 FNS_CLOUD_SOURCES" % path)

    profiles = doc.get("profiles") or []
    replaced = False
    for i, p in enumerate(profiles):
        if p.get("id") == profile["id"]:
            profiles[i] = profile
            replaced = True
            break
    if not replaced:
        profiles.append(profile)

    doc["profiles"] = profiles
    today = time.strftime("%Y-%m-%d")
    doc["rev"] = bump_rev(doc.get("rev"), today)
    doc["issuedAt"] = int(time.time())

    # 内容变了，旧签名必然失效：直接摘掉，免得客户端一直报「签名校验失败」
    if "signature" in doc:
        doc.pop("signature")
        print("⚠ 清单原有签名已失效并被移除，改完请重新跑 tools/sign_cloud.py")

    write_json(path, doc, mode=0o644)
    print("已%s %s → %s（rev=%s，共 %d 组）" % (
        "更新" if replaced else "新增", profile["id"], path, doc["rev"], len(profiles)))
    return doc


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="123 云盘开放平台 → fnspeed 云端清单")
    ap.add_argument("--client-id", help="开放平台应用的 clientID")
    ap.add_argument("--client-secret", help="开放平台应用的 clientSecret")
    ap.add_argument("--token", help="直接用现成的 access_token（跳过换取）")

    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("token", help="换取并缓存 access_token")

    p_link = sub.add_parser("link", help="打印文件直链")
    p_link.add_argument("file_id", help="云盘中的 fileID（数字）")

    p_prof = sub.add_parser("profile", help="打印可直接粘进 sources.json 的配置集")
    p_prof.add_argument("file_id")
    p_prof.add_argument("--id", dest="pid", required=True, help="配置集 id，如 pan123-big")
    p_prof.add_argument("--name", help="显示名")
    p_prof.add_argument("--group", default="自建", help="下拉分组，默认「自建」")
    p_prof.add_argument("--desc", help="说明")
    p_prof.add_argument("--mode", default="down", choices=["down", "up", "both"])

    p_add = sub.add_parser("add", help="取直链并写进清单（自动改 rev）")
    p_add.add_argument("file_id")
    p_add.add_argument("--id", dest="pid", required=True)
    p_add.add_argument("--name")
    p_add.add_argument("--group", default="自建")
    p_add.add_argument("--desc")
    p_add.add_argument("--mode", default="down", choices=["down", "up", "both"])
    p_add.add_argument("-f", "--file", default="cloud/sources.json", help="清单路径")

    args = ap.parse_args()

    if args.cmd == "token":
        tok = get_token(args, force=True)
        exp = jwt_exp(tok)
        print("access_token 就绪" + ("（exp=%s）" % time.strftime("%Y-%m-%d %H:%M", time.localtime(exp)) if exp else ""))
        return

    token = get_token(args)

    if args.cmd == "link":
        print(direct_link(token, args.file_id))
        return

    url = direct_link(token, args.file_id)
    profile = build_profile(url, args.pid, args.name, args.group, args.desc, args.mode)

    if args.cmd == "profile":
        print(json.dumps(profile, ensure_ascii=False, indent=2))
        return

    if args.cmd == "add":
        if not os.path.exists(args.file):
            die("清单文件不存在：%s（先跑一次 build 或从 cloud/sources.json 复制）" % args.file)
        add_to_sources(args.file, profile)
        return


if __name__ == "__main__":
    main()
