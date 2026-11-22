#!/usr/bin/env python3
"""为 .s2plugin 包签名（Ed25519），并打印可写入 plugins.trusted_publishers 的公钥。

用法：
    sign.py <package.s2plugin> [key_path]

key_path 不存在时会生成一个新的 Ed25519 私钥（raw 32 字节，base64 存储，权限 600）。
签名结果写入包内 signature.json：
    {"algorithm":"ed25519","key_id":"<key_id>","signature":"<base64>"}
key_id 默认由公钥 SHA-256 前 16 位派生，可用 S2PLUGIN_KEY_ID 覆盖。
"""
import base64
import hashlib
import json
import os
import sys
import zipfile

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


def load_or_create(path):
    if os.path.exists(path):
        raw = open(path, "rb").read().strip()
        try:
            seed = base64.b64decode(raw, validate=True)
        except Exception as exc:
            raise SystemExit(
                f"私钥文件 {path} 不是合法 base64：{exc}（应为 base64 编码的 32 字节 Ed25519 私钥）"
            )
        if len(seed) != 32:
            raise SystemExit(
                f"私钥文件 {path} 解码后必须是 32 字节，实际 {len(seed)} 字节"
            )
        return Ed25519PrivateKey.from_private_bytes(seed)
    key = Ed25519PrivateKey.generate()
    seed = key.private_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PrivateFormat.Raw,
        encryption_algorithm=serialization.NoEncryption(),
    )
    os.makedirs(os.path.dirname(os.path.abspath(path)) or ".", exist_ok=True)
    with open(path, "wb") as f:
        f.write(base64.b64encode(seed))
    os.chmod(path, 0o600)
    print(f"[sign] 已生成新私钥: {path}（请离线备份，丢失后无法再发布同 ID 更新）")
    return key


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    pkg = argv[1]
    key_path = argv[2] if len(argv) > 2 else os.path.expanduser("~/.sub4api/plugin-signing.key")
    key = load_or_create(key_path)

    pub_raw = key.public_key().public_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PublicFormat.Raw,
    )
    pub_b64 = base64.b64encode(pub_raw).decode()
    key_id = os.environ.get("S2PLUGIN_KEY_ID") or "publisher-" + hashlib.sha256(pub_raw).hexdigest()[:16]

    with zipfile.ZipFile(pkg) as zf:
        manifest_raw = zf.read("manifest.json")
        entries = [(i, zf.read(i.filename)) for i in zf.infolist() if i.filename != "signature.json"]

    signature = {
        "algorithm": "ed25519",
        "key_id": key_id,
        "signature": base64.b64encode(key.sign(manifest_raw)).decode(),
    }
    tmp = pkg + ".tmp"
    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as out:
        for info, data in entries:
            out.writestr(info, data)
        out.writestr("signature.json", json.dumps(signature, ensure_ascii=False, indent=2))
    os.replace(tmp, pkg)

    print(f"[sign] 已签名: {pkg}")
    print(f"[sign] key_id: {key_id}")
    print("[sign] 把下面一行写入 config 的 plugins.trusted_publishers：")
    print(f'  {key_id}: "{pub_b64}"')
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
