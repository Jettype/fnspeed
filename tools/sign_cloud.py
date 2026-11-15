#!/usr/bin/env python3
"""给云配置清单算 HMAC-SHA256 签名，写回 signature 字段。

签名原文是纯文本行协议（见 docs/CLOUD.md 第 3 节），与 Go 端的 CloudSign()
逐字节一致：

    FNS_CLOUD_SOURCES
    1
    <rev>
    PROFILE<TAB><id>
    DOWN<TAB><url>
    UP<TAB><url>
    ...

用法：
    # 用 etc/cloud-secret 里的密钥签名，原地写回
    ./tools/sign_cloud.py cloud/sources.json

    # 指定密钥与输出文件
    ./tools/sign_cloud.py cloud/sources.json --secret-file ~/my.key -o dist/sources.json

    # 只校验现有签名，不写文件
    ./tools/sign_cloud.py cloud/sources.json --verify

退出码：0 成功/校验通过，1 校验失败或参数错误。
"""

import argparse
import hashlib
import hmac
import json
import os
import sys


def endpoint_urls(profile, kind):
    """按「纯 URL 列表在前、endpoints 在后」合并，顺序即签名顺序。

    kind 取 "download" 或 "upload"，对应字段名 downloadUrls / downloadEndpoints。
    """
    out = []
    for u in profile.get(kind + "Urls") or []:
        u = (u or "").strip()
        if u:
            out.append(u)
    for e in profile.get(kind + "Endpoints") or []:
        u = (e.get("url") or "").strip()
        if u:
            out.append(u)
    return out


def canonical(doc):
    """生成签名原文（bytes）。改这里就必须同步改 src/cloud.go 的 cloudCanonical。"""
    lines = [
        doc.get("format") or "",
        str(int(doc.get("version") or 0)),
        doc.get("rev") or "",
    ]
    for p in doc.get("profiles") or []:
        lines.append("PROFILE\t" + (p.get("id") or "").strip())
        for u in endpoint_urls(p, "download"):
            lines.append("DOWN\t" + u)
        for u in endpoint_urls(p, "upload"):
            lines.append("UP\t" + u)
    return "".join(line + "\n" for line in lines).encode("utf-8")


def sign(doc, secret):
    return hmac.new(secret, canonical(doc), hashlib.sha256).hexdigest()


def load_secret(args):
    if args.secret:
        return args.secret.encode("utf-8")
    path = args.secret_file
    if not os.path.exists(path):
        sys.exit(
            "找不到密钥文件 %s\n"
            "用 --secret 直接给，或 --secret-file 指定路径，或先创建该文件：\n"
            "  printf '%%s' 'your-shared-secret' > %s && chmod 600 %s" % (path, path, path)
        )
    with open(path, "rb") as f:
        key = f.read().strip()
    if not key:
        sys.exit("密钥文件 %s 是空的" % path)
    return key


def main():
    ap = argparse.ArgumentParser(description="给云配置清单算 HMAC-SHA256 签名")
    ap.add_argument("path", help="清单文件路径，如 cloud/sources.json")
    ap.add_argument("--secret", help="直接指定共享密钥（会留在 shell 历史里，慎用）")
    ap.add_argument("--secret-file", default="etc/cloud-secret", help="密钥文件，默认 etc/cloud-secret")
    ap.add_argument("-o", "--out", help="输出路径，默认原地覆盖")
    ap.add_argument("--verify", action="store_true", help="只校验现有签名，不写文件")
    args = ap.parse_args()

    with open(args.path, "r", encoding="utf-8") as f:
        doc = json.load(f)

    if doc.get("format") != "FNS_CLOUD_SOURCES":
        sys.exit('format 应为 "FNS_CLOUD_SOURCES"，实际 %r' % doc.get("format"))

    secret = load_secret(args)
    want = sign(doc, secret)

    if args.verify:
        got = (doc.get("signature") or "").strip().lower()
        if got.startswith("hmac-sha256="):
            got = got[len("hmac-sha256="):]
        if not got:
            sys.exit("清单里没有 signature 字段")
        if not hmac.compare_digest(want, got):
            sys.exit("签名不匹配\n  文件里: %s\n  按当前内容应为: %s" % (got, want))
        print("签名有效：%s" % got)
        return

    doc["signature"] = want
    out = args.out or args.path
    tmp = out + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="\n") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, out)
    print("已签名 → %s" % out)
    print("signature: %s" % want)


if __name__ == "__main__":
    main()
