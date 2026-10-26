// Known-answer tests for the pure-JS primitives: BLAKE2b, Argon2, ChaCha20, Salsa20.
// BLAKE2b and ChaCha20 extra vectors were computed with Python hashlib / cryptography (independent
// implementations); RFC vectors are copied from the RFC text.
import test from 'node:test';
import assert from 'node:assert/strict';
import { blake2b, Blake2b } from '../../assets/js/lib/vault/blake2b.js';
import { argon2, argon2Sync } from '../../assets/js/lib/vault/argon2.js';
import { chacha20, chacha20Block, ChaCha20 } from '../../assets/js/lib/vault/chacha20.js';
import { Salsa20 } from '../../assets/js/lib/vault/salsa20.js';
import { fromHex, toHex, utf8Encode, sha256 } from '../../assets/js/lib/vault/bytes.js';

const range = (n, f = (i) => i) => Uint8Array.from({ length: n }, (_, i) => f(i));

test('BLAKE2b RFC 7693 Appendix A and reference vectors', () => {
  assert.equal(
    toHex(blake2b(utf8Encode('abc'))),
    'ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d17d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923',
  );
  assert.equal(
    toHex(blake2b(new Uint8Array(0))),
    '786a02f742015903c6c6fd852552d272912f4740e15847618a86e217f71f5419d25e1031afee585313896444934eb04b903a685b1448b755d56f701afe9be2ce',
  );
  const key = range(64);
  // blake2b-kat.txt entries 0, 128 and 255 (keyed)
  assert.equal(
    toHex(blake2b(new Uint8Array(0), 64, key)),
    '10ebb67700b1868efb4417987acf4690ae9d972fb7a590c2f02871799aaa4786b5e996e8f0f4eb981fc214b005f42d2ff4233499391653df7aefcbc13fc51568',
  );
  assert.equal(
    toHex(blake2b(range(128), 64, key)),
    '72065ee4dd91c2d8509fa1fc28a37c7fc9fa7d5b3f8ad3d0d7a25626b57b1b44788d4caf806290425f9890a3a2a35a905ab4b37acfd0da6e4517b2525c9651e4',
  );
  assert.equal(
    toHex(blake2b(range(255), 64, key)),
    '142709d62e28fcccd0af97fad0f8465b971e82201dc51070faa0372aa43e92484be1c1e73ba10906d5d1853db6a4106e0a7bf9800d373d6dee2d46d62ef2a461',
  );
  assert.equal(toHex(blake2b(utf8Encode('abc'), 32)), 'bddd813c634239723171ef3fee98579b94964e3bb1cb3e427262c8c068d52319');
  assert.equal(toHex(blake2b(utf8Encode('abc'), 1)), '6b');
  assert.equal(
    toHex(blake2b(range(1000, (i) => (i * 7) & 255), 48, utf8Encode('hello'))),
    '91acce2fd4f981c13e38b91cb354156a3848614c2d84aecae912e87e7c168bd1c241c556f22d4512713cc54ef112bccb',
  );
  assert.equal(
    toHex(blake2b(new Uint8Array(256))),
    'ec9c6b301a6c98946d742a74710e658f0243e0e6d3525f4afa94dfc2395456fa54ebe5ef0f413b5a9abfe6501dabb4b9a0fbca164d6cd80b1e79dbbed8d4202e',
  );
  assert.equal(
    toHex(blake2b(range(129))),
    'f59711d44a031d5f97a9413c065d1e614c417ede998590325f49bad2fd444d3e4418be19aec4e11449ac1a57207898bc57d76a1bcf3566292c20c683a5c4648f',
  );
});

test('BLAKE2b incremental updates equal one-shot for every split point', () => {
  const data = range(300, (i) => (i * 31) & 255);
  const want = toHex(blake2b(data, 64, utf8Encode('k')));
  for (const split of [0, 1, 64, 127, 128, 129, 200, 256, 299, 300]) {
    const h = new Blake2b(64, utf8Encode('k'));
    h.update(data.subarray(0, split));
    h.update(data.subarray(split));
    assert.equal(toHex(h.digest()), want, `split ${split}`);
  }
  assert.throws(() => new Blake2b(65), /output length/);
  assert.throws(() => new Blake2b(64, new Uint8Array(65)), /key/);
});

// RFC 9106 §5: P = 32×0x01, S = 16×0x02, K = 8×0x03, X = 12×0x04, m = 32 KiB, t = 3, p = 4, T = 32.
const rfc = {
  password: new Uint8Array(32).fill(1),
  salt: new Uint8Array(16).fill(2),
  secret: new Uint8Array(8).fill(3),
  ad: new Uint8Array(12).fill(4),
  memory: 32,
  iterations: 3,
  parallelism: 4,
  hashLength: 32,
};

test('Argon2 RFC 9106 §5 test vectors (d, i, id)', () => {
  assert.equal(toHex(argon2Sync({ ...rfc, type: 'argon2d' })), '512b391b6f1162975371d30919734294f868e3be3984f3c1a13a4db9fabe4acb');
  assert.equal(toHex(argon2Sync({ ...rfc, type: 'argon2i' })), 'c814d9d1dc7f37aa13f0d77f2494bda1c8de6b016dd388d29952a4c4672b6ce8');
  assert.equal(toHex(argon2Sync({ ...rfc, type: 'argon2id' })), '0d640df58d78766c08c037a34a8b53c9d01ef0452d75b65eb52520e96b01e659');
});

test('Argon2 phc-winner reference vectors (v0x13 and legacy v0x10, argon2i)', () => {
  const base = { password: 'password', salt: 'somesalt', iterations: 2, parallelism: 1, hashLength: 32, type: 'argon2i' };
  assert.equal(toHex(argon2Sync({ ...base, memory: 256 })), '89e9029f4637b295beb027056a7336c414fadd43f6b208645281cb214a56452f');
  assert.equal(toHex(argon2Sync({ ...base, memory: 256, parallelism: 2 })), '4ff5ce2769a1d7f4c8a491df09d41a9fbe90e5eb02155a13e4c01e20cd4eab61');
  assert.equal(toHex(argon2Sync({ ...base, memory: 256, version: 0x10 })), 'fd4dd83d762c49bdeaf57c47bdcd0c2f1babf863fdeb490df63ede9975fccf06');
  assert.equal(
    toHex(argon2Sync({ ...base, memory: 256, parallelism: 2, version: 0x10 })),
    'b6c11560a6a9d61eac706b79a2f97d68b4463aa3ad87e00c07e2b01e90c564fb',
  );
});

test('Argon2 async equals sync, reports progress, validates input', async () => {
  const opts = { password: 'pw', salt: 'saltsalt', memory: 64, iterations: 2, parallelism: 2, type: 'argon2id' };
  const seen = [];
  const a = await argon2({ ...opts, onProgress: (f) => seen.push(f) });
  assert.deepEqual(a, argon2Sync(opts));
  assert.equal(seen.at(-1), 1);
  // Output lengths > 64 exercise the H' chaining path.
  assert.equal(argon2Sync({ ...opts, hashLength: 100 }).length, 100);
  assert.throws(() => argon2Sync({ ...opts, salt: 'short' }), /salt/);
  assert.throws(() => argon2Sync({ ...opts, memory: 8 }), /memory/);
  assert.throws(() => argon2Sync({ ...opts, version: 0x12 }), /version/);
  assert.throws(() => argon2Sync({ ...opts, type: 'argon2x' }), /type/);
});

test('Argon2 speed: 64 MiB, t=2, p=2 (KeePass/spec defaults)', () => {
  const t0 = performance.now();
  const out = argon2Sync({ password: 'x', salt: new Uint8Array(32), memory: 65536, iterations: 2, parallelism: 2, type: 'argon2d' });
  const ms = performance.now() - t0;
  assert.equal(out.length, 32);
  console.log(`# argon2d 64MiB t=2 p=2: ${ms.toFixed(0)} ms`);
  assert.ok(ms < 15000, `argon2 too slow: ${ms} ms`);
});

test('ChaCha20 RFC 8439 §2.3.2 block and §2.4.2 encryption vectors', () => {
  const key = range(32);
  assert.equal(
    toHex(chacha20Block(key, fromHex('000000090000004a00000000'), 1)),
    '10f1e7e4d13b5915500fdd1fa32071c4c7d1f4c733c068030422aa9ac3d46c4ed2826446079faa0914c2d705d98b02a2b5129cd1de164eb9cbd083e8a2503c4e',
  );
  const pt = utf8Encode(
    "Ladies and Gentlemen of the class of '99: If I could offer you only one tip for the future, sunscreen would be it.",
  );
  const ct = chacha20(key, fromHex('000000000000004a00000000'), pt, 1);
  assert.equal(
    toHex(ct),
    '6e2e359a2568f98041ba0728dd0d6981e97e7aec1d4360c20a27afccfd9fae0bf91b65c5524733ab8f593dabcd62b3571639d624e65152ab8f530c359f0861d807ca0dbf500d6a6156a38e088a22b65e52bc514d16ccf806818ce91ab77937365af90bbf74a35be6b40b8eedf2785e42874d',
  );
  assert.deepEqual(chacha20(key, fromHex('000000000000004a00000000'), ct, 1), pt);
});

test('ChaCha20 matches Python cryptography and streams across calls', async () => {
  const data = range(300, (i) => (i * 13) & 255);
  assert.equal(
    toHex(chacha20(range(32), fromHex('000000000000004a00000000'), data, 5)),
    'cb5caf8529fc76538402d3640599659b8f1cd030b351ad2630430853be38a73e91d20d857400b78695dc37403ca103c6b9a9522b67059efcface4953c18b5f0ec9b08d571f094ef6d35ed6cbdc75a6b877f913bb70c52c479febe2503492c3b0daf1ec8df2740c18e1f085d66c9f0970abdd92cfa7141484a1830d002074726c9a1638730fafba5594d58c42740814d7de620722bc39796ef512c99fa20c41f397f6dfe3436878f3903cefa9a4bb56defef78a25e321e38d699fe7365334e78578414073d5e2a9a7c95e3dc327635bffb1e7b335b303c0750c9370db3c43998b73efad348a3ed9e8a025d9f952338ba075c0f204f2c3c25a2ea9b661e6fa9053e13326b8d24aff4a227e9fe8d4b9e9c42077a51273b0f0d44a9e1f3bdaef59fb1c3f4c459c165aca35759c9a',
  );
  const s = new ChaCha20(range(32, (i) => 100 + i), range(12));
  const parts = [s.keystream(1), s.keystream(63), s.keystream(65), s.keystream(871)];
  const all = new Uint8Array(1000);
  let o = 0;
  for (const p of parts) {
    all.set(p, o);
    o += p.length;
  }
  assert.equal(toHex(await sha256(all)), '1092f62af14be9cdfeea4e90e891326fe46afa746993b536412d0f490fa6df16');
  assert.throws(() => new ChaCha20(new Uint8Array(16), new Uint8Array(12)), /key/);
  assert.throws(() => new ChaCha20(new Uint8Array(32), new Uint8Array(8)), /nonce/);
});

test('Salsa20/20 eSTREAM set 1 vector 0 (256-bit key) and streaming', () => {
  const key = new Uint8Array(32);
  key[0] = 0x80;
  const s = new Salsa20(key, new Uint8Array(8));
  assert.equal(
    toHex(s.keystream(64)).toUpperCase(),
    'E3BE8FDD8BECA2E3EA8EF9475B29A6E7003951E1097A5C38D23B7A5FAD9F6844B22C97559E2723C7CBBD3FE4FC8D9A0744652A83E72A9C461876AF4D7EF1A117',
  );
  const a = new Salsa20(range(32), range(8)).keystream(200);
  const b = new Salsa20(range(32), range(8));
  const joined = new Uint8Array([...b.keystream(7), ...b.keystream(100), ...b.keystream(93)]);
  assert.deepEqual(joined, a);
});
