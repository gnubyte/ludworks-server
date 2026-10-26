#!/usr/bin/env python3
"""Independent cross-check of the Ludworks vault reference implementation (web/assets/js/lib/vault).

Uses only the Python standard library plus the `cryptography` package, and shares no code with the JS:

  (a) recomputes every value in docs/spec/vault-test-vectors.json (PBKDF2 via hashlib, HKDF by hand,
      AES-GCM via cryptography, TOTP via hmac, the generator algorithm from its written description);
  (b) parses and decrypts the KDBX 4 files written by the JS writer (web/tests/vault/fixtures/),
      verifying the header SHA-256, header HMAC, every block HMAC, the Argon2id / AES-KDF key
      derivation, the outer cipher, gzip, the inner ChaCha20 stream and the entry values.

Run from ludworks-server/:   python3 -I web/tests/vault/crosscheck.py
Exit status 0 = everything matched.
"""
import base64
import datetime
import gzip
import hashlib
import hmac
import json
import os
import re
import struct
import sys
import unicodedata
import xml.etree.ElementTree as ET

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives import padding
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.argon2 import Argon2id

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER_ROOT = os.path.normpath(os.path.join(HERE, "..", "..", ".."))
FAILURES = []
CHECKS = 0


def check(cond, what):
    global CHECKS
    CHECKS += 1
    if not cond:
        FAILURES.append(what)
        print(f"FAIL: {what}")


def b64(b):
    return base64.b64encode(b).decode("ascii")


def sha256(b):
    return hashlib.sha256(b).digest()


def sha512(b):
    return hashlib.sha512(b).digest()


# --------------------------------------------------------------------------------------------------
# (a) vault-test-vectors.json
# --------------------------------------------------------------------------------------------------

def hkdf_sha256(ikm, salt, info, length):
    prk = hmac.new(salt if salt else b"\x00" * 32, ikm, hashlib.sha256).digest()
    okm, t, i = b"", b"", 1
    while len(okm) < length:
        t = hmac.new(prk, t + info + bytes([i]), hashlib.sha256).digest()
        okm += t
        i += 1
    return okm[:length]


def seal(key, nonce, plaintext, aad):
    return "v1." + b64(nonce) + "." + b64(AESGCM(key).encrypt(nonce, plaintext, aad.encode("utf-8")))


def open_sealed(key, sealed, aad):
    parts = sealed.split(".")
    if len(parts) != 3:
        return "malformed", None
    if parts[0] != "v1":
        return "unsupported_version", None
    nonce, ct = base64.b64decode(parts[1], validate=True), base64.b64decode(parts[2], validate=True)
    try:
        return None, AESGCM(key).decrypt(nonce, ct, aad.encode("utf-8"))
    except InvalidTag:
        return "decrypt_failed", None


def totp(secret_b32, algorithm, digits, period, unix_time):
    s = secret_b32.replace(" ", "").upper().rstrip("=")
    key = base64.b32decode(s + "=" * (-len(s) % 8))
    digest = {"SHA1": hashlib.sha1, "SHA256": hashlib.sha256, "SHA512": hashlib.sha512}[algorithm]
    mac = hmac.new(key, struct.pack(">Q", unix_time // period), digest).digest()
    off = mac[-1] & 0x0F
    return str((int.from_bytes(mac[off:off + 4], "big") & 0x7FFFFFFF) % (10 ** digits)).zfill(digits)


class TestStream:
    """SHA-256(seed || LE32(i)) blocks; draws are little-endian uint32 values."""

    def __init__(self, seed):
        self.seed, self.counter, self.buf = seed, 0, b""

    def u32(self):
        while len(self.buf) < 4:
            self.buf += sha256(self.seed + struct.pack("<I", self.counter))
            self.counter += 1
        v, self.buf = struct.unpack("<I", self.buf[:4])[0], self.buf[4:]
        return v

    def randint(self, n):
        limit = 2 ** 32 - (2 ** 32 % n)
        while True:
            x = self.u32()
            if x < limit:
                return x % n


def gen_password(opts, cs, stream):
    length = opts.get("length", 20)
    amb = cs["ambiguous"] if opts.get("excludeAmbiguous") else ""
    sets = []
    for name in ("upper", "lower", "digits"):
        if opts.get(name, True):
            sets.append("".join(c for c in cs[name] if c not in amb))
    if opts.get("symbols", True):
        sets.append(cs["symbols"])
    pool = "".join(sets)
    while True:
        pw = "".join(pool[stream.randint(len(pool))] for _ in range(length))
        if all(any(c in s for c in pw) for s in sets):
            return pw


def load_wordlist():
    src = open(os.path.join(SERVER_ROOT, "web", "assets", "js", "lib", "vault", "wordlist.js"), encoding="utf-8").read()
    raw = re.search(r"const RAW = `(.*?)`;", src, re.S).group(1)
    return raw.split()


def check_vectors():
    v = json.load(open(os.path.join(SERVER_ROOT, "docs", "spec", "vault-test-vectors.json"), encoding="utf-8"))
    k = v["kdf"]
    salt = bytes.fromhex(k["kdf_salt_hex"])
    check(base64.b64decode(k["kdf_salt"]) == salt, "kdf_salt base64 matches hex")
    pw = unicodedata.normalize("NFC", k["password"]).encode("utf-8")
    master = hashlib.pbkdf2_hmac("sha256", pw, salt, k["kdf_iterations"], 32)
    check(master.hex() == k["master_key_hex"], "master_key (PBKDF2-HMAC-SHA256)")
    wrap = hkdf_sha256(master, b"", k["hkdf_info"].encode(), 32)
    check(wrap.hex() == k["wrap_key_hex"], "wrap_key (HKDF-SHA256, empty salt)")

    n = v["nfc"]
    nfc = unicodedata.normalize("NFC", n["password"]).encode("utf-8")
    check(nfc.hex() == n["password_nfc_utf8_hex"], "NFC-normalised password bytes")
    check(n["password"].encode("utf-8") != nfc, "NFC vector input is not already normalised")
    check(hashlib.pbkdf2_hmac("sha256", nfc, bytes.fromhex(n["kdf_salt_hex"]), n["kdf_iterations"], 32).hex() == n["master_key_hex"], "NFC master_key")

    w = v["wrapped_key"]
    vault_key = bytes.fromhex(w["vault_key_hex"])
    check(seal(wrap, bytes.fromhex(w["nonce_hex"]), vault_key, w["aad"]) == w["sealed"], "wrapped_key sealed string")
    check(w["server_params"]["wrapped_key"] == w["sealed"] and w["server_params"]["kdf_salt"] == k["kdf_salt"], "server_params consistent")
    err, pt = open_sealed(wrap, w["sealed"], w["aad"])
    check(err is None and pt == vault_key, "unwrap vault key")

    it = v["item"]
    check(it["aad"] == "ludworks/vault/item/v1:" + it["id"], "item AAD format")
    check(seal(vault_key, bytes.fromhex(it["nonce_hex"]), it["plaintext_json"].encode("utf-8"), it["aad"]) == it["data"], "item ciphertext")
    json.loads(it["plaintext_json"])
    f = v["folder"]
    check(f["aad"] == "ludworks/vault/folder/v1:" + f["id"], "folder AAD format")
    check(json.loads(f["plaintext_json"]) == {"name": f["name"]}, "folder plaintext JSON")
    check(seal(vault_key, bytes.fromhex(f["nonce_hex"]), f["plaintext_json"].encode("utf-8"), f["aad"]) == f["sealed"], "folder ciphertext")
    for case in v["open_must_fail"]:
        err, _ = open_sealed(bytes.fromhex(case["key_hex"]), case["sealed"], case["aad"])
        check(err == case["error"], f"open must fail: {case['case']}")
    kc = v["key_check"]
    check(kc["aad"] == "ludworks/vault/check/v1" and kc["vault_key_hex"] == w["vault_key_hex"], "key_check AAD and key")
    check(seal(vault_key, bytes.fromhex(kc["nonce_hex"]), b"", kc["aad"]) == kc["sealed"], "key_check sealed string")
    err, pt = open_sealed(vault_key, kc["sealed"], kc["aad"])
    check(err is None and pt == b"", "key_check opens to the empty string")

    for t in v["totp"]:
        check(totp(t["secret_base32"], t["algorithm"], t["digits"], t["period"], t["unix_time"]) == t["code"], f"TOTP {t['algorithm']} @ {t['unix_time']}")
    rfc = {(59, "SHA1"): "94287082", (59, "SHA256"): "46119246", (59, "SHA512"): "90693936",
           (20000000000, "SHA1"): "65353130", (20000000000, "SHA256"): "77737706", (20000000000, "SHA512"): "47863826"}
    for t in v["totp"]:
        if (t["unix_time"], t["algorithm"]) in rfc and t["digits"] == 8:
            check(t["code"] == rfc[(t["unix_time"], t["algorithm"])], f"RFC 6238 appendix B {t['algorithm']} @ {t['unix_time']}")
    for o in v["otpauth"]:
        p = o["parsed"]
        check(o["uri"].startswith("otpauth://totp/") and re.fullmatch(r"[A-Z2-7]+", p["secret"]) is not None, f"otpauth parse {o['uri'][:40]}")

    g = v["generator"]
    seed = g["seed_utf8"].encode("utf-8")
    for case in g["passwords"]:
        check(gen_password(case["options"], g["charsets"], TestStream(seed)) == case["password"], f"generator password {case['options']}")
    pp = g["passphrase"]
    words = load_wordlist()
    check(len(words) == pp["wordlist_size"] and len(set(words)) == len(words), "wordlist size / uniqueness")
    s = TestStream(seed)
    idx = [s.randint(len(words)) for _ in range(pp["options"]["words"])]
    check(idx == pp["word_indices"], "passphrase word indices")
    chosen = [words[i] for i in idx]
    if pp["options"].get("capitalize"):
        chosen = [w[0].upper() + w[1:] for w in chosen]
    if pp["options"].get("includeNumber"):
        pos = s.randint(len(chosen))
        chosen[pos] += str(s.randint(10))
    check(pp["options"]["separator"].join(chosen) == pp["passphrase"], "passphrase")

    # The Argon2id RFC 9106 §5.3 constant used by the JS unit tests, confirmed by an independent Argon2.
    tag = Argon2id(salt=b"\x02" * 16, length=32, iterations=3, lanes=4, memory_cost=32, ad=b"\x04" * 12, secret=b"\x03" * 8).derive(b"\x01" * 32)
    check(tag.hex() == "0d640df58d78766c08c037a34a8b53c9d01ef0452d75b65eb52520e96b01e659", "Argon2id RFC 9106 vector")


# --------------------------------------------------------------------------------------------------
# (b) KDBX 4 files written by the JS writer
# --------------------------------------------------------------------------------------------------

CIPHER_AES = bytes.fromhex("31c1f2e6bf714350be5805216afc5aff")
CIPHER_CHACHA = bytes.fromhex("d6038a2b8b6f4cb5a524339a31dbb59a")
KDF_AES = bytes.fromhex("c9d9f39a628a4460bf740d08c18a4fea")
KDF_ARGON2ID = bytes.fromhex("9e298b1956db4773b23dfc3ec6f0a1e6")


def parse_variant_dict(b):
    assert b[1] == 0x01, "VariantDictionary major version"
    pos, out = 2, {}
    while True:
        t = b[pos]
        pos += 1
        if t == 0:
            return out
        nlen = struct.unpack_from("<i", b, pos)[0]
        name = b[pos + 4:pos + 4 + nlen].decode("utf-8")
        pos += 4 + nlen
        vlen = struct.unpack_from("<i", b, pos)[0]
        raw = b[pos + 4:pos + 4 + vlen]
        pos += 4 + vlen
        out[name] = {0x04: lambda r: struct.unpack("<I", r)[0], 0x05: lambda r: struct.unpack("<Q", r)[0], 0x42: lambda r: r}[t](raw)


def keyfile_key(data):
    root = ET.fromstring(data.decode("utf-8"))
    assert root.tag == "KeyFile" and root.find("Meta/Version").text.strip() == "2.0"
    node = root.find("Key/Data")
    key = bytes.fromhex("".join(node.text.split()))
    check(sha256(key)[:4].hex().upper() == node.get("Hash").upper(), "key file v2 hash")
    return key


def kdbx_time(text):
    secs = struct.unpack("<q", base64.b64decode(text))[0]
    t = datetime.datetime(1, 1, 1, tzinfo=datetime.timezone.utc) + datetime.timedelta(seconds=secs)
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def read_kdbx4(path, password, keyfile):
    data = open(path, "rb").read()
    sig1, sig2, minor, major = struct.unpack_from("<IIHH", data, 0)
    check((sig1, sig2, major) == (0x9AA2D903, 0xB54BFB67, 4), f"{os.path.basename(path)}: KDBX 4 signature")
    pos, fields = 12, {}
    while True:
        fid, size = data[pos], struct.unpack_from("<I", data, pos + 1)[0]
        val = data[pos + 5:pos + 5 + size]
        pos += 5 + size
        if fid == 0:
            break
        fields[fid] = val
    header = data[:pos]
    check(sha256(header) == data[pos:pos + 32], "header SHA-256")

    parts = [sha256(password.encode("utf-8"))]
    if keyfile is not None:
        parts.append(keyfile_key(keyfile))
    composite = sha256(b"".join(parts))
    kdf = parse_variant_dict(fields[11])
    if kdf["$UUID"] == KDF_ARGON2ID:
        check(kdf["V"] == 0x13, "Argon2 version 0x13")
        transformed = Argon2id(salt=kdf["S"], length=32, iterations=kdf["I"], lanes=kdf["P"], memory_cost=kdf["M"] // 1024).derive(composite)
        kdf_desc = {"type": "argon2id", "memory": kdf["M"], "iterations": kdf["I"], "parallelism": kdf["P"]}
    elif kdf["$UUID"] == KDF_AES:
        enc = Cipher(algorithms.AES(kdf["S"]), modes.ECB()).encryptor()
        halves = [composite[:16], composite[16:]]
        for _ in range(kdf["R"]):
            halves = [enc.update(h) for h in halves]
        transformed = sha256(halves[0] + halves[1])
        kdf_desc = {"type": "aes", "rounds": kdf["R"]}
    else:
        raise AssertionError("unexpected KDF")
    seed = fields[4]
    master = sha256(seed + transformed)
    hmac_base = sha512(seed + transformed + b"\x01")

    def block_key(i):
        return sha512(struct.pack("<Q", i) + hmac_base)

    check(hmac.compare_digest(hmac.new(block_key(2 ** 64 - 1), header, hashlib.sha256).digest(), data[pos + 32:pos + 64]), "header HMAC")
    pos += 64
    ct, idx = b"", 0
    while True:
        mac, size = data[pos:pos + 32], struct.unpack_from("<i", data, pos + 32)[0]
        blk = data[pos + 36:pos + 36 + size]
        pos += 36 + size
        good = hmac.compare_digest(hmac.new(block_key(idx), struct.pack("<QI", idx, size) + blk, hashlib.sha256).digest(), mac)
        check(good, f"block {idx} HMAC")
        if size == 0:
            break
        ct += blk
        idx += 1
    check(pos == len(data), "no trailing data after the final block")

    iv = fields[7]
    if fields[2] == CIPHER_AES:
        dec = Cipher(algorithms.AES(master), modes.CBC(iv)).decryptor()
        unpad = padding.PKCS7(128).unpadder()
        plain = unpad.update(dec.update(ct) + dec.finalize()) + unpad.finalize()
        cipher = "aes"
    elif fields[2] == CIPHER_CHACHA:
        plain = Cipher(algorithms.ChaCha20(master, b"\x00" * 4 + iv), None).decryptor().update(ct)
        cipher = "chacha20"
    else:
        raise AssertionError("unexpected cipher")
    if struct.unpack("<I", fields[3])[0] == 1:
        plain = gzip.decompress(plain)

    ipos, stream_id, stream_key = 0, None, None
    while True:
        fid, size = plain[ipos], struct.unpack_from("<i", plain, ipos + 1)[0]
        val = plain[ipos + 5:ipos + 5 + size]
        ipos += 5 + size
        if fid == 0:
            break
        if fid == 1:
            stream_id = struct.unpack("<I", val)[0]
        elif fid == 2:
            stream_key = val
    check(stream_id == 3, "inner stream is ChaCha20 (id 3)")
    h = sha512(stream_key)
    stream = Cipher(algorithms.ChaCha20(h[:32], b"\x00" * 4 + h[32:44]), None).encryptor()
    text = plain[ipos:].decode("utf-8")
    check("<!DOCTYPE" not in text, "no DOCTYPE in payload")
    root = ET.fromstring(text)
    for el in root.iter():  # document order, which is the order the stream must be consumed in
        if el.get("Protected") == "True":
            el.text = stream.update(base64.b64decode(el.text or "")).decode("utf-8")
    return root, cipher, kdf_desc


def entries_of(root):
    out = []

    def entry(e, path):
        fields, prot = {}, []
        for s in e.findall("String"):
            key, val = s.find("Key").text or "", s.find("Value")
            fields[key] = val.text or ""
            if val.get("Protected") == "True":
                prot.append(key)
        hist = [h.find("String[Key='Password']/Value").text or "" for h in e.findall("History/Entry")]
        return {"path": path, "fields": fields, "protected": prot, "history_passwords": hist,
                "creation_time": kdbx_time(e.find("Times/CreationTime").text),
                "modification_time": kdbx_time(e.find("Times/LastModificationTime").text)}

    def group(g, path):
        for e in g.findall("Entry"):
            out.append(entry(e, path))
        for c in g.findall("Group"):
            name = c.find("Name").text or ""
            group(c, f"{path}/{name}" if path else name)

    group(root.find("Root/Group"), "")
    return out


def check_kdbx():
    fx = os.path.join(HERE, "fixtures")
    expected = json.load(open(os.path.join(fx, "crosscheck-expected.json"), encoding="utf-8"))
    norm = lambda es: sorted(es, key=lambda e: (e["path"], e["fields"].get("Title", "")))
    for f in expected["fixtures"]:
        keyfile = open(os.path.join(fx, f["keyfile"]), "rb").read() if f["keyfile"] else None
        root, cipher, kdf = read_kdbx4(os.path.join(fx, f["file"]), f["password"], keyfile)
        name = f["file"]
        check(cipher == f["cipher"], f"{name}: cipher {cipher}")
        check(kdf == f["kdf"], f"{name}: KDF parameters {kdf}")
        check(root.find("Root/Group/Name").text == expected["database_name"], f"{name}: root group name")
        check(norm(entries_of(root)) == norm(expected["entries"]), f"{name}: entries match")


if __name__ == "__main__":
    check_vectors()
    check_kdbx()
    if FAILURES:
        print(f"{len(FAILURES)} of {CHECKS} checks FAILED")
        sys.exit(1)
    print(f"crosscheck OK: {CHECKS} checks passed")
