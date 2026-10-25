#!/usr/bin/env python3
"""打包 Vault 的 GitHub Release 产物。

默认产出两个 zip（与 build.py --package-all 的结果一致）：

    release/v0.1.0-beta/
      Vault-Server-0.1.0-beta-win64.zip
      Vault-Client-0.1.0-beta-win64.zip
      SHA256SUMS.txt
      RELEASE_NOTES.md

脚本只负责**准备**产物，不会自动发布。上传时把两个 zip 拖进 GitHub Release，
或者直接复制脚本最后打印的 `gh release create` 命令。

版本号有两套，互不影响：
  - 对外版本：RELEASE.json（本脚本读写，界面展示与发布产物命名）
  - 内部构建号：VERSION / package.json（build.py 自动递增，自动更新与兼容性判定）

常用用法：
    python release.py
    python release.py --version 0.2.0 --channel beta
    python release.py --skip-build
    python release.py --with-linux
    python release.py --build-arg=--no-bump
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
RELEASE_FILE = ROOT / "RELEASE.json"
BUILD_SCRIPT = ROOT / "build.py"
VERSION_FILE = ROOT / "VERSION"
PACKAGE_DIR = ROOT / "dist" / "package"

SEMVER_RE = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
CHANNELS = ("stable", "beta", "rc", "alpha")


def die(message: str, code: int = 1) -> None:
    print(f"\n[失败] {message}", file=sys.stderr)
    raise SystemExit(code)


def info(message: str) -> None:
    print(message)


def load_release() -> dict:
    if not RELEASE_FILE.is_file():
        die(f"缺少 {RELEASE_FILE.name}。请先创建：\n"
            '  {\n    "version": "0.1.0",\n    "channel": "beta"\n  }')
    try:
        data = json.loads(RELEASE_FILE.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        die(f"{RELEASE_FILE.name} 不是合法 JSON：{exc}")
    version = str(data.get("version", "")).strip()
    channel = str(data.get("channel", "stable")).strip().lower() or "stable"
    if not SEMVER_RE.match(version):
        die(f"{RELEASE_FILE.name} 的 version 非法（需形如 1.2.3）：{version!r}")
    if channel not in CHANNELS:
        die(f"{RELEASE_FILE.name} 的 channel 非法：{channel!r}（可选：{', '.join(CHANNELS)}）")
    return {"version": version, "channel": channel}


def save_release(release: dict) -> None:
    RELEASE_FILE.write_text(
        json.dumps(release, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def display_version(release: dict) -> str:
    """对外展示/文件命名用：0.1.0-beta；stable 不带后缀。"""
    if release["channel"] == "stable":
        return release["version"]
    return f'{release["version"]}-{release["channel"]}'


def release_tag(release: dict) -> str:
    return f'v{display_version(release)}'


def read_internal_version() -> str:
    if not VERSION_FILE.is_file():
        die("缺少 VERSION 文件，无法确定内部构建号")
    return VERSION_FILE.read_text(encoding="utf-8").strip()


def git_commit() -> str:
    try:
        out = subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
            check=True,
        )
        return out.stdout.strip() or "unknown"
    except Exception:
        return "unknown"


def run_build(args: argparse.Namespace) -> None:
    cmd = [sys.executable, str(BUILD_SCRIPT), "--package-all"]
    # 发布脚本默认绝不允许临时生成新密钥：否则产物会用一把只存在于本机的私钥签名，
    # 客户端内置的公钥对不上，用户更新必然验签失败。
    if not args.allow_keygen:
        cmd.append("--no-keygen")
    if args.no_bump:
        cmd.append("--no-bump")
    if args.key:
        cmd += ["--key", args.key]
    cmd += args.build_arg

    info("\n执行构建：" + " ".join(cmd))
    result = subprocess.run(cmd, cwd=str(ROOT))
    if result.returncode != 0:
        die(f"build.py 执行失败（退出码 {result.returncode}）")


def find_artifact(pattern: str, description: str) -> Path:
    matches = sorted(PACKAGE_DIR.glob(pattern), key=lambda p: p.stat().st_mtime, reverse=True)
    if not matches:
        die(f"找不到{description}：{PACKAGE_DIR / pattern}\n"
            "请确认已完整执行构建（不要带 --skip-go / --skip-frontend），且客户端没有在运行。")
    return matches[0]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{int(value)} B"
        value /= 1024
    return f"{size} B"


def write_notes(out_dir: Path, release: dict, internal: str, artifacts: list[Path]) -> Path:
    tag = release_tag(release)
    title = f'Vault {release["version"]}' + (
        "" if release["channel"] == "stable" else f' {release["channel"].upper()}'
    )
    lines = [
        f"# {title}",
        "",
        f"- Tag: `{tag}`",
        f"- Internal build: `{internal}`",
        f"- Commit: `{git_commit()}`",
        f"- Built at: {time.strftime('%Y-%m-%d %H:%M:%S')}",
        "",
        "## Highlights",
        "",
        "- ",
        "",
        "## Downloads",
        "",
        "| File | Size | SHA-256 |",
        "| --- | --- | --- |",
    ]
    for path in artifacts:
        lines.append(f"| `{path.name}` | {human_size(path.stat().st_size)} | `{sha256(path)}` |")
    lines += [
        "",
        "> Full checksums are available in `SHA256SUMS.txt`.",
        "",
    ]
    notes = out_dir / "RELEASE_NOTES.md"
    notes.write_text("\n".join(lines), encoding="utf-8")
    return notes


def print_publish_hint(out_dir: Path, release: dict, artifacts: list[Path], notes: Path) -> None:
    tag = release_tag(release)
    title = f'Vault {release["version"]}' + (
        "" if release["channel"] == "stable" else f' {release["channel"].upper()}'
    )
    flags = " --prerelease" if release["channel"] != "stable" else ""
    files = " ".join(f'"{p}"' for p in artifacts)
    info("\n产物已就绪。可手动上传，或执行：")
    info(
        f'gh release create {tag} {files} '
        f'--title "{title}" --notes-file "{notes}"{flags}'
    )
    info(f"\nGitHub 仓库：https://github.com/junqirao/vault/releases")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="release.py",
        description="构建并整理 Vault 的 GitHub Release 产物（不会自动发布）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--version", help="设置对外版本（形如 0.1.0），会写回 RELEASE.json")
    parser.add_argument("--channel", choices=CHANNELS, help="设置发布渠道，会写回 RELEASE.json")
    parser.add_argument("--skip-build", action="store_true", help="跳过构建，只整理 dist/package 里已有的产物")
    parser.add_argument("--with-linux", action="store_true", help="额外收集 Linux 服务端 tar.gz")
    parser.add_argument("--out", help="产物输出目录（默认 release/<tag>）")
    parser.add_argument("--key", help="发布方签名私钥路径（透传给 build.py）")
    parser.add_argument("--allow-keygen", action="store_true",
                        help="允许 build.py 在缺少密钥时临时生成（仅本地测试，切勿用于正式发布）")
    parser.add_argument("--no-bump", action="store_true",
                        help="内部构建号不自动递增（透传给 build.py）")
    parser.add_argument("--build-arg", action="append", default=[],
                        help="额外透传给 build.py 的参数，可重复使用")
    args = parser.parse_args(argv)

    release = load_release()
    if args.version:
        version = args.version.strip()
        if not SEMVER_RE.match(version):
            die(f"--version 非法（需形如 1.2.3）：{version!r}")
        release["version"] = version
    if args.channel:
        release["channel"] = args.channel
    save_release(release)

    tag = release_tag(release)
    out_dir = Path(args.out) if args.out else ROOT / "release" / tag
    if not out_dir.is_absolute():
        out_dir = ROOT / out_dir
    if out_dir.exists():
        shutil.rmtree(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    info(f"对外版本：{display_version(release)}（tag {tag}）")
    info(f"输出目录：{out_dir}")

    if args.skip_build:
        info("\n已跳过构建，使用现有 dist/package 产物")
    else:
        run_build(args)

    internal = read_internal_version()
    info(f"内部构建号：{internal}")

    artifacts: list[Path] = []
    server_zip = find_artifact(f"vault-server-{internal}-win64.zip", "Windows 服务端 zip")
    client_zip = find_artifact(f"vault-client-{internal}-win64.zip", "Windows 客户端 zip")
    pairs = [
        (server_zip, f"Vault-Server-{display_version(release)}-win64.zip"),
        (client_zip, f"Vault-Client-{display_version(release)}-win64.zip"),
    ]
    if args.with_linux:
        linux_tgz = find_artifact(
            f"vault-server-{internal}-linux-amd64.tar.gz", "Linux 服务端 tar.gz"
        )
        pairs.append((linux_tgz, f"Vault-Server-{display_version(release)}-linux-amd64.tar.gz"))

    for source, target_name in pairs:
        target = out_dir / target_name
        shutil.copy2(source, target)
        artifacts.append(target)

    checksum_lines = [f"{sha256(path)}  {path.name}" for path in artifacts]
    (out_dir / "SHA256SUMS.txt").write_text("\n".join(checksum_lines) + "\n", encoding="utf-8")
    notes = write_notes(out_dir, release, internal, artifacts)

    info("\nRelease 产物：")
    for path in artifacts:
        info(f"  {path.name}  ({human_size(path.stat().st_size)})")
    info(f"  {notes.name}")
    info(f"  SHA256SUMS.txt")
    print_publish_hint(out_dir, release, artifacts, notes)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\n已中断", file=sys.stderr)
        sys.exit(130)
