#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Windows（无 bash）环境下的等价打包实现，产物结构与 build.sh 逐项对齐。

用法：
    python tools/pack_fpk.py              # 交叉编译双架构 + 打包
    python tools/pack_fpk.py --no-build   # 复用 .build/<arch>/target/fnspeed

与 build.sh 的差别（都是刻意的）：
  * 不重新生成图标 —— 那个步骤会把 ICON*.PNG / ui/icon.png 重写一遍，弄脏工作树；
    这里直接沿用仓库里的 ICON.PNG / ICON_256.PNG（与已发布包同源）。
  * 不编译 sink —— sink 不进 fpk，build.sh 里那一步只是本机自测用。
  * tar 条目显式写权限位：cmd/* 0755，其余 0644，目录 0755。
    bsdtar 在 Windows 上会把所有文件记成 0666（没有 x 位），
    装到 fnOS 上 cmd/main 就起不来。

版本号取自 build.sh 的 VER，不在这里再写一份 —— 三处同步（build.sh / manifest /
CHANGELOG）是项目约定，多一个来源只会多一处能写歪的地方。
"""

import hashlib
import io
import os
import re
import shutil
import subprocess
import sys
import tarfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = os.path.join(ROOT, "src")
BUILD = os.path.join(ROOT, ".build")
DIST = os.path.join(ROOT, "dist")

APP = "fnspeed"
UNIX_ATTR = dict(uid=0, gid=0, uname="root", gname="root")


def read_ver():
    with open(os.path.join(ROOT, "build.sh"), encoding="utf-8") as f:
        m = re.search(r'^VER="([^"]+)"', f.read(), re.M)
    if not m:
        sys.exit("build.sh 里找不到 VER=")
    return m.group(1)


def go_bin():
    """定位 go：环境变量 FNSPEED_GO > 用户工具链目录 > PATH。

    不写死绝对路径 —— 仓库里的文件不允许出现本机目录（隐私门禁会拦），
    换机器或换用户名也不该让打包脚本失效。
    """
    env = os.environ.get("FNSPEED_GO")
    if env and os.path.exists(env):
        return env
    cand = os.path.expanduser(r"~/.workbuddy/binaries/go/go/bin/go.exe")
    if os.path.exists(cand):
        return cand
    found = shutil.which("go")
    if not found:
        sys.exit("找不到 go：设 FNSPEED_GO 指向 go.exe，或把 go 加入 PATH")
    return found


def build_binary(arch, ver, stage):
    env = dict(os.environ)
    env.update(GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
    out = os.path.join(stage, APP)
    ldflags = "-s -w -X main.appVersion=" + ver
    cmd = [go_bin(), "build", "-trimpath", "-ldflags=" + ldflags, "-o", out, "."]
    print("==> 编译 %s：%s" % (arch, " ".join(cmd)))
    subprocess.run(cmd, cwd=SRC, env=env, check=True)


def add_file(tf, path, arcname, mode):
    ti = tf.gettarinfo(path, arcname)
    ti.mode = mode
    ti.uid = UNIX_ATTR["uid"]
    ti.gid = UNIX_ATTR["gid"]
    ti.uname = UNIX_ATTR["uname"]
    ti.gname = UNIX_ATTR["gname"]
    with open(path, "rb") as f:
        tf.addfile(ti, f)


def add_dir(tf, arcname, mtime):
    ti = tarfile.TarInfo(arcname)
    ti.type = tarfile.DIRTYPE
    ti.mode = 0o755
    ti.uid = UNIX_ATTR["uid"]
    ti.gid = UNIX_ATTR["gid"]
    ti.uname = UNIX_ATTR["uname"]
    ti.gname = UNIX_ATTR["gname"]
    ti.mtime = mtime
    tf.addfile(ti)


def make_app_tgz(stage, out):
    """app.tgz 内部平铺：./fnspeed、./ui/config、./ui/images/*。"""
    mtime = int(os.path.getmtime(stage))
    with tarfile.open(out, "w:gz") as tf:
        add_dir(tf, "./", mtime)
        add_file(tf, os.path.join(stage, APP), "./" + APP, 0o644)
        add_dir(tf, "./ui/", mtime)
        add_file(tf, os.path.join(stage, "ui", "config"), "./ui/config", 0o644)
        add_dir(tf, "./ui/images/", mtime)
        for name in sorted(os.listdir(os.path.join(stage, "ui", "images"))):
            add_file(tf, os.path.join(stage, "ui", "images", name), "./ui/images/" + name, 0o644)


def make_fpk(pkg, out):
    """fpk 顶层：manifest app.tgz cmd config ui ICON.PNG ICON_256.PNG fnspeed.sc。"""
    mtime = int(os.path.getmtime(pkg))
    with tarfile.open(out, "w:gz") as tf:
        add_file(tf, os.path.join(pkg, "manifest"), "manifest", 0o644)
        add_file(tf, os.path.join(pkg, "app.tgz"), "app.tgz", 0o644)
        for d in ("cmd", "config"):
            add_dir(tf, d + "/", mtime)
            for name in sorted(os.listdir(os.path.join(pkg, d))):
                mode = 0o755 if d == "cmd" else 0o644
                add_file(tf, os.path.join(pkg, d, name), d + "/" + name, mode)
        add_dir(tf, "ui/", mtime)
        add_file(tf, os.path.join(pkg, "ui", "config"), "ui/config", 0o644)
        add_dir(tf, "ui/images/", mtime)
        for name in sorted(os.listdir(os.path.join(pkg, "ui", "images"))):
            add_file(tf, os.path.join(pkg, "ui", "images", name), "ui/images/" + name, 0o644)
        for name in ("ICON.PNG", "ICON_256.PNG", APP + ".sc"):
            add_file(tf, os.path.join(pkg, name), name, 0o644)


def write_manifest(ver, platform, checksum, out):
    """按行改写 key = value，保留原有对齐与字段顺序。"""
    with open(os.path.join(ROOT, "manifest"), encoding="utf-8") as f:
        lines = f.read().splitlines()
    want = {"appname": APP, "version": ver, "platform": platform, "checksum": checksum}
    seen = set()
    for i, ln in enumerate(lines):
        m = re.match(r"^(\w+)(\s+)=(\s*)(.*)$", ln)
        if not m:
            continue
        key = m.group(1)
        if key not in want:
            continue
        seen.add(key)
        lines[i] = "%s%s=%s%s" % (key, m.group(2), m.group(3), want[key])
    missing = set(want) - seen
    if missing:
        sys.exit("manifest 缺少字段：%s" % ", ".join(sorted(missing)))
    with open(out, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines) + "\n")


def md5(path):
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def build_pkg(arch, platform, ver, no_build):
    stage = os.path.join(BUILD, arch, "target")
    os.makedirs(os.path.join(stage, "ui", "images"), exist_ok=True)
    binary = os.path.join(stage, APP)
    if no_build and os.path.exists(binary):
        print("==> %s：复用已有二进制" % arch)
    else:
        build_binary(arch, ver, stage)

    print("==> 组装 %s" % arch)
    shutil.copy2(os.path.join(ROOT, "ui", "config"), os.path.join(stage, "ui", "config"))
    img = os.path.join(stage, "ui", "images")
    for src, dst in (
        ("ICON.PNG", "icon_64.png"),
        ("ICON_256.PNG", "icon_256.png"),
        ("ICON.PNG", "icon-64.png"),
        ("ICON_256.PNG", "icon-256.png"),
    ):
        shutil.copy2(os.path.join(ROOT, src), os.path.join(img, dst))

    apptgz = os.path.join(BUILD, "app-%s.tgz" % arch)
    make_app_tgz(stage, apptgz)

    pkg = os.path.join(BUILD, "pkg-%s" % arch)
    shutil.rmtree(pkg, ignore_errors=True)
    for d in ("cmd", "config", "ui/images"):
        os.makedirs(os.path.join(pkg, d), exist_ok=True)
    shutil.copy2(apptgz, os.path.join(pkg, "app.tgz"))
    for name in os.listdir(os.path.join(ROOT, "cmd")):
        shutil.copy2(os.path.join(ROOT, "cmd", name), os.path.join(pkg, "cmd", name))
    for name in os.listdir(os.path.join(ROOT, "config")):
        shutil.copy2(os.path.join(ROOT, "config", name), os.path.join(pkg, "config", name))
    shutil.copy2(os.path.join(ROOT, "ui", "config"), os.path.join(pkg, "ui", "config"))
    for name in os.listdir(img):
        shutil.copy2(os.path.join(img, name), os.path.join(pkg, "ui", "images", name))
    for name in ("ICON.PNG", "ICON_256.PNG", APP + ".sc"):
        shutil.copy2(os.path.join(ROOT, name), os.path.join(pkg, name))

    checksum = md5(os.path.join(pkg, "app.tgz"))
    write_manifest(ver, platform, checksum, os.path.join(pkg, "manifest"))

    out = os.path.join(DIST, "%s_%s_%s.fpk" % (APP, platform, ver))
    make_fpk(pkg, out)
    size = os.path.getsize(out)
    print("    产出 %s (%.2f MB) md5(app.tgz)=%s" % (out, size / 1048576.0, checksum))


def main():
    ver = read_ver()
    no_build = "--no-build" in sys.argv[1:]
    os.makedirs(BUILD, exist_ok=True)
    os.makedirs(DIST, exist_ok=True)
    for arch, platform in (("amd64", "x86"), ("arm64", "arm")):
        build_pkg(arch, platform, ver, no_build)
    print("\n==> 完成：%s" % DIST)
    for name in sorted(os.listdir(DIST)):
        if name.endswith(".fpk"):
            print("    %-32s %.2f MB" % (name, os.path.getsize(os.path.join(DIST, name)) / 1048576.0))


if __name__ == "__main__":
    main()
