"""本地打包：与 .github/workflows/release.yml 的产物约定保持一致。

三个不能省的细节（都是踩过的）：
  1. zip 条目名必须用正斜杠 —— PowerShell 5.1 的 Compress-Archive 会写
     反斜杠，Linux/macOS 解压出一个叫 "dir\\file" 的单文件。
  2. 桌面壳包带 README.txt，**不带** start.bat（后者是命令行服务端的双击入口）。
  3. 校验和用 sha256sum 形态（`<hash>  <name>`），不带 binary 模式的 `*` 前缀。
"""

import hashlib
import os
import shutil
import tarfile
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PKG = os.path.join(ROOT, "pkg")
OUT = os.path.join(ROOT, "out")

README = """NaiBox 桌面壳（Windows amd64）
================================

双击 NaiBox.exe 即可启动，无控制台窗口。
首次启动会读取同目录的 config.example.json；把它复制成 config.json
后按需修改（端口、上游、api_key），再重启生效。

网关默认监听 127.0.0.1:7863，启动后浏览器打开：
    http://127.0.0.1:7863/healthz

日志/配置目录：见界面里的「打开数据目录」，或 %APPDATA%\\NaiBox。
卸载：直接删除本目录，不写注册表、不装服务。

校验和见 Release 页的 SHA256SUMS.txt。
"""


def mkzip(src_dir, out_path):
    base = os.path.dirname(src_dir)
    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_DEFLATED) as z:
        for dirpath, _, files in os.walk(src_dir):
            for f in sorted(files):
                full = os.path.join(dirpath, f)
                rel = os.path.relpath(full, base)
                z.write(full, rel.replace(os.sep, "/"))


def mktar(src_dir, out_path):
    """打 tar.gz，条目名与 CI 的 `tar czf ... -C pkg <dir>` 一致。

    用 Python 的 tarfile 而不是 shell 的 tar：Windows Git Bash 的 tar 会把
    `C:` 当成远程主机名解析（老版本），而 Python 版本跨平台没有这个坑。
    """
    base = os.path.dirname(src_dir)
    with tarfile.open(out_path, "w:gz") as t:
        t.add(src_dir, arcname=os.path.basename(src_dir))


def main():
    # 只清理本脚本会产出的文件，不整目录 rm -rf。
    #
    # ⚠ 踩过一次：早先这里 `shutil.rmtree(OUT)`，于是"打 zip + 算校验和"
    # 这一步把上一步 bash 打的 4 个 tar.gz 全删了，校验和里只剩 2 行 ——
    # 产物看起来齐全（zip 在），实际少了一半。清理要按自己负责的范围来。
    os.makedirs(OUT, exist_ok=True)
    for f in os.listdir(OUT):
        if f.endswith((".zip", ".tar.gz")) or f == "SHA256SUMS.txt":
            os.remove(os.path.join(OUT, f))

    # 桌面壳包
    desk = os.path.join(PKG, "NaiBox-windows-amd64")
    os.makedirs(desk, exist_ok=True)
    shutil.copy2(os.path.join(ROOT, "dist", "NaiBox.exe"), desk)
    shutil.copy2(os.path.join(ROOT, "config.example.json"), desk)
    with open(os.path.join(desk, "README.txt"), "w", encoding="utf-8", newline="\n") as fh:
        fh.write(README)

    # zip 包（Windows）
    for name in ("Naibox-server-windows-amd64", "NaiBox-windows-amd64"):
        src = os.path.join(PKG, name)
        if not os.path.isdir(src):
            raise SystemExit("缺少待打包目录: " + src)
        mkzip(src, os.path.join(OUT, name + ".zip"))
        print("zip  " + name)

    # tar.gz 包（linux / darwin）
    for name in ("Naibox-server-linux-amd64", "Naibox-server-linux-arm64",
                 "Naibox-server-darwin-amd64", "Naibox-server-darwin-arm64"):
        src = os.path.join(PKG, name)
        if not os.path.isdir(src):
            raise SystemExit("缺少待打包目录: " + src)
        mktar(src, os.path.join(OUT, name + ".tar.gz"))
        print("tar  " + name)

    lines = []
    for f in sorted(os.listdir(OUT)):
        if f.endswith((".zip", ".tar.gz")):
            with open(os.path.join(OUT, f), "rb") as fh:
                h = hashlib.sha256(fh.read()).hexdigest()
            lines.append("%s  %s" % (h, f))
    with open(os.path.join(OUT, "SHA256SUMS.txt"), "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(lines) + "\n")
    print("\n" + "\n".join(lines))


if __name__ == "__main__":
    main()
