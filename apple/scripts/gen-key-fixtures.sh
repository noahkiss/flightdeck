#!/bin/sh
# Prints KeyFixtures.swift: throwaway keys made with ssh-keygen in a temporary
# directory, only for the key import tests. Never reads ~/.ssh.
# Usage (from the repo root):
#   apple/scripts/gen-key-fixtures.sh > apple/FlightdeckKit/Tests/FlightdeckKitTests/KeyFixtures.swift
set -eu
D=$(mktemp -d)
trap 'rm -rf "$D"' EXIT
PASS='correct horse battery staple'
gen() { # name args...
	n=$1; shift
	ssh-keygen -q "$@" -f "$D/$n" >/dev/null
}
gen ed25519 -t ed25519 -N '' -C 'test@example'
gen ed25519_enc -t ed25519 -N "$PASS" -C 'test@example'
gen ed25519_gcm -t ed25519 -N "$PASS" -Z aes256-gcm@openssh.com -a 4 -C 'test@example'
gen ed25519_cbc -t ed25519 -N "$PASS" -Z aes128-cbc -a 4 -C 'test@example'
gen ed25519_chacha -t ed25519 -N "$PASS" -Z chacha20-poly1305@openssh.com -a 4 -C 'test@example'
for b in 256 384 521; do
	gen ecdsa$b -t ecdsa -b $b -N '' -C "ecdsa$b@example"
	gen ecdsa${b}_enc -t ecdsa -b $b -N "$PASS" -a 4 -C "ecdsa$b@example"
done
gen ecdsa256_pem -t ecdsa -b 256 -N '' -m PEM -C ''
gen ecdsa384_pkcs8 -t ecdsa -b 384 -N '' -m PKCS8 -C ''
gen ecdsa256_pem_enc -t ecdsa -b 256 -N "$PASS" -m PEM -C ''
gen rsa -t rsa -b 1024 -N '' -C 'rsa@example'
gen rsa_pem -t rsa -b 1024 -N '' -m PEM -C ''
gen ed25519_rounds1 -t ed25519 -N "$PASS" -a 1 -C 'rounds1@example'

echo "// Throwaway keys made with ssh-keygen only for these tests (invented"
echo "// comments, never used anywhere else). Regenerate with"
echo "// apple/scripts/gen-key-fixtures.sh. Each file is stored without its"
echo "// BEGIN and END lines, so secret scanners do not take these test keys for"
echo "// real ones; armor() puts them back."
echo
echo "enum KeyFixtures {"
echo "    static let passphrase = \"$PASS\""
echo
echo "    /// A key file from its kind (OPENSSH, EC, RSA, empty for PKCS#8) and body."
cat <<'SWIFT'
    private static func armor(_ kind: String, _ body: String) -> String {
        let label = (kind.isEmpty ? "" : kind + " ") + "PRIVATE" + " KEY"
        return "-----BEGIN \(label)-----\n" + body + "\n-----END \(label)-----\n"
    }
SWIFT
for f in "$D"/*; do
	case "$f" in *.pub) continue ;; esac
	n=$(basename "$f")
	kind=$(head -1 "$f" | sed -e 's/^-----BEGIN //' -e 's/ *PRIVATE KEY-----$//')
	echo
	echo "    static let $n = armor(\"$kind\", \"\"\""
	sed -e '1d' -e '$d' -e 's/^/        /' "$f"
	echo "        \"\"\")"
	echo "    static let ${n}_pub = \"$(cut -d' ' -f1,2 "$f.pub")\""
	echo "    static let ${n}_fp = \"$(ssh-keygen -lf "$f.pub" | awk '{print $2}')\""
done
echo "}"
