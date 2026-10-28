#!/usr/bin/env python3
"""Generates srp_vectors.json, the SRP-6a known-answer vectors of the AirPlay driver tests.

This is an independent implementation (Python standard library only) of the SRP-6a variant
HomeKit Accessory Protocol pair-setup uses: the RFC 5054 3072-bit group with g = 5, SHA-512,
k = H(N | PAD(g)), x = H(salt | H(I ":" P)), u = H(PAD(A) | PAD(B)), K = H(S without leading
zero bytes), M1 = H(H(N) xor H(g) | H(I) | salt | A | B | K) with A and B stripped of leading
zero bytes, M2 = H(PAD(A) | M1 | K) (the conventions of Apple's HomeKit ADK).

Before writing anything it checks itself against
  * RFC 5054 Appendix B (1024-bit group, SHA-1: k, x, v, A, B, u, premaster secret), and
  * the 3072-bit prime's defining formula 2^3072 - 2^3008 - 1 + 2^64 * ([2^2942 pi] + 1690314).
The "alice" HAP vector uses the RFC 5054 inputs (salt, a, b, alice/password123); its values
equal the SRP test vectors of Apple's HomeKit ADK (Tests/HAPCryptoTest.c).

Usage: python3 -I gen_srp_vectors.py > srp_vectors.json
"""

import hashlib
import json
import sys


def h2i(s):
    return int("".join(s.split()), 16)


# RFC 5054 Appendix A, group 1 (1024-bit, g = 2) and group 4 (3072-bit, g = 5).
N1024 = h2i(
    """EEAF0AB9 ADB38DD6 9C33F80A FA8FC5E8 60726187 75FF3C0B 9EA2314C
    9C256576 D674DF74 96EA81D3 383B4813 D692C6E0 E0D5D8E2 50B98BE4
    8E495C1D 6089DAD1 5DC7D7B4 6154D6B6 CE8EF4AD 69B15D49 82559B29
    7BCF1885 C529F566 660E57EC 68EDBC3C 05726CC0 2FD4CBF4 976EAA9A
    FD5138FE 8376435B 9FC61D2F C0EB06E3"""
)
N3072 = h2i(
    """FFFFFFFF FFFFFFFF C90FDAA2 2168C234 C4C6628B 80DC1CD1 29024E08
    8A67CC74 020BBEA6 3B139B22 514A0879 8E3404DD EF9519B3 CD3A431B
    302B0A6D F25F1437 4FE1356D 6D51C245 E485B576 625E7EC6 F44C42E9
    A637ED6B 0BFF5CB6 F406B7ED EE386BFB 5A899FA5 AE9F2411 7C4B1FE6
    49286651 ECE45B3D C2007CB8 A163BF05 98DA4836 1C55D39A 69163FA8
    FD24CF5F 83655D23 DCA3AD96 1C62F356 208552BB 9ED52907 7096966D
    670C354E 4ABC9804 F1746C08 CA18217C 32905E46 2E36CE3B E39E772C
    180E8603 9B2783A2 EC07A28F B5C55DF0 6F4C52C9 DE2BCBF6 95581718
    3995497C EA956AE5 15D22618 98FA0510 15728E5A 8AAAC42D AD33170D
    04507A33 A85521AB DF1CBA64 ECFB8504 58DBEF0A 8AEA7157 5D060C7D
    B3970F85 A6E1E4C7 ABF5AE8C DB0933D7 1E8C94E0 4A25619D CEE3D226
    1AD2EE6B F12FFA06 D98A0864 D8760273 3EC86A64 521F2B18 177B200C
    BBE11757 7A615D6C 770988C0 BAD946E2 08E24FA0 74E5AB31 43DB5BFC
    E0FD108E 4B82D120 A93AD2CA FFFFFFFF FFFFFFFF"""
)

# RFC 5054 Appendix B.
RFC_SALT = bytes.fromhex("BEB25379D1A8581EB5A727673A2441EE")
RFC_A_PRIV = h2i("60975527 035CF2AD 1989806F 0407210B C81EDC04 E2762A56 AFD529DD DA2D4393")
RFC_B_PRIV = h2i("E487CB59 D31AC550 471E81F0 0F6928E0 1DDA08E9 74A004F4 9E61F5D1 05284D20")
RFC_EXPECT = {
    "k": h2i("7556AA04 5AEF2CDD 07ABAF0F 665C3E81 8913186F"),
    "x": h2i("94B7555A ABE9127C C58CCF49 93DB6CF8 4D16C124"),
    "v": h2i(
        """7E273DE8 696FFC4F 4E337D05 B4B375BE B0DDE156 9E8FA00A 9886D812
        9BADA1F1 822223CA 1A605B53 0E379BA4 729FDC59 F105B478 7E5186F5
        C671085A 1447B52A 48CF1970 B4FB6F84 00BBF4CE BFBB1681 52E08AB5
        EA53D15C 1AFF87B2 B9DA6E04 E058AD51 CC72BFC9 033B564E 26480D78
        E955A5E2 9E7AB245 DB2BE315 E2099AFB"""
    ),
    "A": h2i(
        """61D5E490 F6F1B795 47B0704C 436F523D D0E560F0 C64115BB 72557EC4
        4352E890 3211C046 92272D8B 2D1A5358 A2CF1B6E 0BFCF99F 921530EC
        8E393561 79EAE45E 42BA92AE ACED8251 71E1E8B9 AF6D9C03 E1327F44
        BE087EF0 6530E69F 66615261 EEF54073 CA11CF58 58F0EDFD FE15EFEA
        B349EF5D 76988A36 72FAC47B 0769447B"""
    ),
    "B": h2i(
        """BD0C6151 2C692C0C B6D041FA 01BB152D 4916A1E7 7AF46AE1 05393011
        BAF38964 DC46A067 0DD125B9 5A981652 236F99D9 B681CBF8 7837EC99
        6C6DA044 53728610 D0C6DDB5 8B318885 D7D82C7F 8DEB75CE 7BD4FBAA
        37089E6F 9C6059F3 88838E7A 00030B33 1EB76840 910440B1 B27AAEAE
        EB4012B7 D7665238 A8E3FB00 4B117B58"""
    ),
    "u": h2i("CE38B959 3487DA98 554ED47D 70A7AE5F 462EF019"),
    "S": h2i(
        """B0DC82BA BCF30674 AE450C02 87745E79 90A3381F 63B387AA F271A10D
        233861E3 59B48220 F7C4693C 9AE12B0A 6F67809F 0876E2D0 13800D6C
        41BB59B6 D5979B5C 00A172B4 A2A5903A 0BDCAF8A 709585EB 2AFAFA8F
        3499B200 210DCC1F 10EB3394 3CD67FC8 8A2F39A4 BE5BEC4E C0A3212D
        C346D7E4 74B29EDE 8A469FFE CA686E5A"""
    ),
}


def pi_scaled(bits):
    """floor(pi * 2^bits) with Machin's formula in integer arithmetic."""
    guard = 64
    one = 1 << (bits + guard)

    def arctan_inv(x):
        term = one // x
        total, x2, n, sign = term, x * x, 1, -1
        while term:
            term //= x2
            n += 2
            total += sign * (term // n)
            sign = -sign
        return total

    return (16 * arctan_inv(5) - 4 * arctan_inv(239)) >> guard


def nbytes(n):
    return (n.bit_length() + 7) // 8


def pad(n, length):
    return n.to_bytes(length, "big")


def strip(n):
    return n.to_bytes(nbytes(n), "big")


def H(hf, *parts):
    h = hf()
    for p in parts:
        h.update(p)
    return h.digest()


def i(b):
    return int.from_bytes(b, "big")


def srp(hf, N, g, user, password, salt, a, b):
    """Both sides of one SRP-6a exchange; returns every intermediate value."""
    L = nbytes(N)
    k = i(H(hf, pad(N, L), pad(g, L)))
    x = i(H(hf, salt, H(hf, user + b":" + password)))
    v = pow(g, x, N)
    A = pow(g, a, N)
    B = (k * v + pow(g, b, N)) % N
    u = i(H(hf, pad(A, L), pad(B, L)))
    s_client = pow((B - k * pow(g, x, N)) % N, a + u * x, N)
    s_server = pow(A * pow(v, u, N) % N, b, N)
    assert s_client == s_server, "client and server premaster secrets differ"
    S = s_client
    K = H(hf, strip(S))
    hn, hg = H(hf, strip(N)), H(hf, strip(g))
    m1 = H(hf, bytes(p ^ q for p, q in zip(hn, hg)), H(hf, user), salt, strip(A), strip(B), K)
    m2 = H(hf, pad(A, L), m1, K)
    return dict(k=k, x=x, v=v, A=A, B=B, u=u, S=S, K=K, M1=m1, M2=m2, L=L)


def vector(name, user, password, salt, a, b):
    r = srp(hashlib.sha512, N3072, 5, user, password, salt, a, b)
    L = r["L"]
    return {
        "name": name,
        "user": user.decode(),
        "password": password.decode(),
        "salt": salt.hex(),
        "a": pad(a, 32).hex(),
        "b": pad(b, 32).hex(),
        "v": pad(r["v"], L).hex(),
        "A": pad(r["A"], L).hex(),
        "B": pad(r["B"], L).hex(),
        "u": r["u"].to_bytes(64, "big").hex(),
        "S": pad(r["S"], L).hex(),
        "K": r["K"].hex(),
        "M1": r["M1"].hex(),
        "M2": r["M2"].hex(),
    }


def seeded(label, counter=0):
    return i(hashlib.sha512(f"{label}/{counter}".encode()).digest()[:32])


def search(label, user, password, salt, want):
    """Finds deterministic private keys whose exchange has the wanted leading-zero property."""
    for n in range(100000):
        a, b = seeded(label + "/a", n), seeded(label + "/b", n)
        r = srp(hashlib.sha512, N3072, 5, user, password, salt, a, b)
        if want(r):
            return a, b
    raise SystemExit(f"{label}: no candidate found")


def main():
    # Self-checks before emitting anything.
    formula = (1 << 3072) - (1 << 3008) - 1 + (1 << 64) * (pi_scaled(2942) + 1690314)
    assert formula == N3072, "3072-bit prime does not match its defining formula"
    r = srp(hashlib.sha1, N1024, 2, b"alice", b"password123", RFC_SALT, RFC_A_PRIV, RFC_B_PRIV)
    for key, want in RFC_EXPECT.items():
        assert r[key] == want, f"RFC 5054 {key} mismatch"

    L = nbytes(N3072)
    out = {
        "comment": "SRP-6a known-answer vectors; generated by gen_srp_vectors.py (python3 stdlib).",
        "rfc5054": {
            "comment": "RFC 5054 Appendix B: 1024-bit group, g=2, SHA-1",
            "user": "alice",
            "password": "password123",
            "salt": RFC_SALT.hex(),
            "a": pad(RFC_A_PRIV, 32).hex(),
            "b": pad(RFC_B_PRIV, 32).hex(),
            **{key: format(val, "x") for key, val in RFC_EXPECT.items()},
        },
        "hap": [],
    }
    hap = out["hap"]
    hap.append(vector("alice (equals the HomeKit ADK test vectors)", b"alice", b"password123", RFC_SALT, RFC_A_PRIV, RFC_B_PRIV))
    hap.append(vector("transient pin", b"Pair-Setup", b"3939", hashlib.sha512(b"salt/1").digest()[:16], seeded("t/a"), seeded("t/b")))

    salt = b"\x00" + hashlib.sha512(b"salt/2").digest()[:15]
    a, b = search("lead-B", b"Pair-Setup", b"1234", salt, lambda r: pad(r["B"], L)[0] == 0)
    hap.append(vector("B and salt with a leading zero byte", b"Pair-Setup", b"1234", salt, a, b))

    salt = hashlib.sha512(b"salt/3").digest()[:16]
    a, b = search("lead-S", b"Pair-Setup", b"5678", salt, lambda r: pad(r["S"], L)[0] == 0)
    hap.append(vector("premaster secret with a leading zero byte", b"Pair-Setup", b"5678", salt, a, b))

    salt = hashlib.sha512(b"salt/4").digest()[:16]
    a, b = search("lead-A", b"Pair-Setup", b"0042", salt, lambda r: pad(r["A"], L)[0] == 0)
    hap.append(vector("A with a leading zero byte", b"Pair-Setup", b"0042", salt, a, b))

    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")


main()
