# Computes vectors.json with Python's hmac module only (no SatBooth code). Run from the repo root:
#   python3 -I webhook/testdata/make_vectors.py > webhook/testdata/vectors.json
# The signed string is the decimal t, a dot, then the body bytes; the header is t=<t>,v1=<hex HMAC-SHA256>.
# Cross-check with openssl (same output as the "v1" of any vector):
#   printf '%s.%s' "$t" "$body" | openssl dgst -sha256 -mac HMAC -macopt hexkey:"$secret_hex"
import hashlib, hmac, json

# Fixed test secrets (not real): hex strings of 32, 48 and 64 bytes.
s32 = bytes(range(32)).hex()
s48 = bytes((7 * i + 3) % 256 for i in range(48)).hex()
s64 = bytes((255 - i) for i in range(64)).hex()

paid = '{"id":"%s","type":"order.paid","order_id":"%s","created_at":%d}'
exp = '{"id":"%s","type":"order.expired","order_id":"%s","created_at":%d}'
cases = [
    (s32, 1800000000, paid % ("0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210", 1800000000)),
    (s32, 1800000123, exp % ("00000000000000000000000000000001", "00000000000000000000000000000002", 1800000100)),
    (s48, 0, "{}"),
    (s48, 1799999999, ""),
    (s64, 4102444800, paid % ("ffffffffffffffffffffffffffffffff", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 4102444800)),
    (s64, 1800000042, '{"note":"café ☃ \\"quoted\\" \\n"}'),
]
out = []
for secret_hex, t, body in cases:
    mac = hmac.new(bytes.fromhex(secret_hex), ("%d." % t).encode() + body.encode("utf-8"), hashlib.sha256).hexdigest()
    out.append({"secret_hex": secret_hex, "t": t, "body": body, "header": "t=%d,v1=%s" % (t, mac)})
print(json.dumps({"command": "python3 -I webhook/testdata/make_vectors.py > webhook/testdata/vectors.json", "vectors": out}, indent=1, ensure_ascii=True))
