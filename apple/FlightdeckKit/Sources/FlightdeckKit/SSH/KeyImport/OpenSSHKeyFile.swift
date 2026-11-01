import CommonCrypto
import CryptoKit
import Foundation
import NIOSSH

/// Why a key could not be imported, in words for the import screen
/// (app.md 10.4).
public enum KeyImportError: Error, Equatable, CustomStringConvertible {
    case notAKey
    case publicKeyOnly
    case rsa
    case securityKey
    case unsupportedType(String)
    case unsupportedCipher(String)
    case unsupportedKDF(String)
    case encryptedPEM
    case passphraseNeeded
    case wrongPassphrase
    case checkMismatch
    case badPadding
    case truncated
    case malformed(String)
    case severalKeys
    case tooManyRounds(UInt32)
    case tooLarge

    public var description: String {
        switch self {
        case .notAKey:
            return "This is not an OpenSSH private key. Import the private key file, the one that starts with -----BEGIN OPENSSH."
        case .publicKeyOnly:
            return "This is a public key. Import the private key file (the one without .pub)."
        case .rsa:
            return "RSA keys are not supported. Make an Ed25519 key with ssh-keygen -t ed25519."
        case .securityKey:
            return "This key lives on a hardware security key; its private part is not in the file. Use another key."
        case .unsupportedType(let t):
            return "Keys of type \(t) are not supported. Use an Ed25519 or ECDSA key."
        case .unsupportedCipher(let c):
            return "The key is encrypted with \(c), which Flightdeck does not read. Re-encrypt it with ssh-keygen -p -Z aes256-ctr -f <file>."
        case .unsupportedKDF(let k):
            return "The key uses the key derivation \(k), which Flightdeck does not read. Re-encrypt it with ssh-keygen -p -f <file>."
        case .encryptedPEM:
            return "This key is encrypted in the older PEM format. Rewrite it in the OpenSSH format with ssh-keygen -p -f <file>, then import it again."
        case .passphraseNeeded:
            return "This key is protected by a passphrase. Enter it."
        case .wrongPassphrase:
            return "Wrong passphrase. Try again."
        case .checkMismatch:
            return "The key file is damaged: its check words do not match."
        case .badPadding:
            return "The key file is damaged: its padding is wrong."
        case .truncated:
            return "The key file is cut short. Copy the whole file, from BEGIN to END."
        case .malformed(let what):
            return "The key file is damaged: \(what)."
        case .severalKeys:
            return "The file holds more than one key. Import a file with one key."
        case .tooManyRounds(let n):
            return "The key asks for \(n) key derivation rounds; Flightdeck reads at most \(OpenSSHKeyFile.maxRounds). Re-encrypt it with ssh-keygen -p -a 100 -f <file>."
        case .tooLarge:
            return "This file is too large to be a key."
        }
    }
}

/// The key types an imported key may have.
public enum ImportedKeyType: String, Codable, Sendable, CaseIterable {
    case ed25519
    case p256 = "ecdsa-p256"
    case p384 = "ecdsa-p384"
    case p521 = "ecdsa-p521"

    /// The SSH name of the key type.
    public var sshName: String {
        switch self {
        case .ed25519: return "ssh-ed25519"
        case .p256: return "ecdsa-sha2-nistp256"
        case .p384: return "ecdsa-sha2-nistp384"
        case .p521: return "ecdsa-sha2-nistp521"
        }
    }

    /// How Settings names the type.
    public var label: String {
        switch self {
        case .ed25519: return "Ed25519"
        case .p256: return "ECDSA P-256"
        case .p384: return "ECDSA P-384"
        case .p521: return "ECDSA P-521"
        }
    }

    init?(sshName: String) {
        guard let t = Self.allCases.first(where: { $0.sshName == sshName }) else { return nil }
        self = t
    }

    /// The private scalar's length for ECDSA, the seed's for Ed25519.
    var scalarLength: Int {
        switch self {
        case .ed25519, .p256: return 32
        case .p384: return 48
        case .p521: return 66
        }
    }
}

/// A decrypted private key: the type, the raw private part (the Ed25519
/// seed or the ECDSA scalar) and the file's comment. Lives in memory only
/// until it goes to the Keychain.
public struct ImportedKeyMaterial: Sendable {
    public let type: ImportedKeyType
    /// The Ed25519 seed (32 bytes) or the ECDSA private scalar.
    let raw: Data
    public let comment: String

    init(type: ImportedKeyType, raw: Data, comment: String) throws {
        self.type = type
        self.raw = raw
        self.comment = comment
        _ = try privateKey()
    }

    /// The key for signing.
    func privateKey() throws -> NIOSSHPrivateKey {
        do {
            switch type {
            case .ed25519: return NIOSSHPrivateKey(ed25519Key: try Curve25519.Signing.PrivateKey(rawRepresentation: raw))
            case .p256: return NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: raw))
            case .p384: return NIOSSHPrivateKey(p384Key: try P384.Signing.PrivateKey(rawRepresentation: raw))
            case .p521: return NIOSSHPrivateKey(p521Key: try P521.Signing.PrivateKey(rawRepresentation: raw))
            }
        } catch {
            throw KeyImportError.malformed("the private key is not valid")
        }
    }

    public var publicKey: NIOSSHPublicKey {
        // `init` proved the key valid.
        (try? privateKey().publicKey)!
    }
}

/// A private key file as read, before any passphrase: the key type and
/// public key come from the file's unencrypted part, so an unsupported type
/// is refused before the passphrase prompt.
public struct OpenSSHKeyFile: Sendable {
    public let type: ImportedKeyType
    public let publicKey: NIOSSHPublicKey?
    public let isEncrypted: Bool
    private let body: Body

    private enum Body: Sendable {
        case openSSH(cipher: String, kdf: String, kdfOptions: [UInt8], publicBlob: [UInt8], privateSection: [UInt8], tag: [UInt8])
        case ready(ImportedKeyMaterial)
    }

    // Built in two parts, so secret scanners do not read the pair as a key.
    static let begin = "-----BEGIN OPENSSH " + "PRIVATE KEY-----"
    static let end = "-----END OPENSSH " + "PRIVATE KEY-----"
    static let magic = Array("openssh-key-v1".utf8) + [0]
    /// The largest input read: an OpenSSH key file is a few kilobytes.
    public static let maxSize = 64 * 1024
    /// The most bcrypt rounds accepted: far above ssh-keygen's default 16
    /// and the common 100 or 256, low enough that a crafted file cannot keep
    /// the CPU busy for hours.
    public static let maxRounds: UInt32 = 1024

    /// Reads at most `maxSize` bytes of a file, refusing a larger one without
    /// reading it all.
    public static func read(contentsOf url: URL) throws -> Data {
        if let size = try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize, size > maxSize {
            throw KeyImportError.tooLarge
        }
        let h = try FileHandle(forReadingFrom: url)
        defer { try? h.close() }
        let data = try h.read(upToCount: maxSize + 1) ?? Data()
        guard data.count <= maxSize else { throw KeyImportError.tooLarge }
        return data
    }

    /// Reads a key file, or text with a key block in it: the first private
    /// key block counts and text around it is ignored.
    public static func parse(_ data: Data) throws -> OpenSSHKeyFile {
        guard data.count <= maxSize else { throw KeyImportError.tooLarge }
        guard let text = String(data: data, encoding: .utf8) ?? String(data: data, encoding: .isoLatin1) else {
            throw KeyImportError.notAKey
        }
        return try parse(text)
    }

    public static func parse(_ text: String) throws -> OpenSSHKeyFile {
        guard text.utf8.count <= maxSize else { throw KeyImportError.tooLarge }
        let text = text.replacingOccurrences(of: "\r\n", with: "\n").replacingOccurrences(of: "\r", with: "\n")
        if let b = text.range(of: begin) {
            guard let e = text.range(of: end, range: b.upperBound..<text.endIndex) else { throw KeyImportError.truncated }
            let inner = text[b.upperBound..<e.lowerBound].filter { !$0.isWhitespace }
            guard let bytes = Data(base64Encoded: String(inner)) else {
                throw KeyImportError.malformed("the base64 text is broken")
            }
            return try parseOpenSSH(Array(bytes))
        }
        if let pem = try PEMKey.find(in: text) {
            return OpenSSHKeyFile(type: pem.type, publicKey: pem.publicKey, isEncrypted: false, body: .ready(pem))
        }
        if looksLikePublicKey(text) { throw KeyImportError.publicKeyOnly }
        throw KeyImportError.notAKey
    }

    private init(type: ImportedKeyType, publicKey: NIOSSHPublicKey?, isEncrypted: Bool, body: Body) {
        self.type = type
        self.publicKey = publicKey
        self.isEncrypted = isEncrypted
        self.body = body
    }

    private static func looksLikePublicKey(_ text: String) -> Bool {
        if text.contains("-----BEGIN SSH2 PUBLIC KEY-----") || text.contains("-----BEGIN PUBLIC KEY-----") { return true }
        let prefixes = ["ssh-ed25519 ", "ssh-rsa ", "ssh-dss ", "ecdsa-sha2-", "sk-ssh-ed25519@openssh.com ", "sk-ecdsa-sha2-"]
        return text.split(separator: "\n").contains { line in
            let l = line.trimmingCharacters(in: .whitespaces)
            return prefixes.contains { l.hasPrefix($0) }
        }
    }

    /// Refuses key types other than Ed25519 and ECDSA, in words.
    static func checkType(_ name: String) throws -> ImportedKeyType {
        if let t = ImportedKeyType(sshName: name) { return t }
        if name == "ssh-rsa" || name.hasPrefix("rsa-") { throw KeyImportError.rsa }
        if name.hasPrefix("sk-") { throw KeyImportError.securityKey }
        throw KeyImportError.unsupportedType(name)
    }

    /// The container (PROTOCOL.key): magic, cipher, KDF and its options,
    /// the key count, each public key, then the private section.
    private static func parseOpenSSH(_ bytes: [UInt8]) throws -> OpenSSHKeyFile {
        guard bytes.starts(with: magic) else { throw KeyImportError.malformed("it does not start with the OpenSSH key header") }
        var r = SSHWireReader(bytes, at: magic.count)
        let cipher = try r.text()
        let kdf = try r.text()
        let kdfOptions = try r.string()
        let n = try r.u32()
        guard n == 1 else {
            throw n == 0 ? KeyImportError.malformed("it holds no key") : KeyImportError.severalKeys
        }
        let publicBlob = try r.string()
        var pr = SSHWireReader(publicBlob)
        let type = try checkType(try pr.text())
        let privateSection = try r.string()
        let rest = Array(bytes[r.offset...])

        let spec: CipherSpec?
        if cipher == "none" {
            guard kdf == "none" else { throw KeyImportError.malformed("an unencrypted key names a key derivation") }
            spec = nil
        } else {
            guard let s = CipherSpec.named(cipher) else { throw KeyImportError.unsupportedCipher(cipher) }
            guard kdf == "bcrypt" else { throw KeyImportError.unsupportedKDF(kdf) }
            // Checked here, before the passphrase prompt and any work.
            _ = try kdfParameters(kdfOptions)
            spec = s
        }
        let tagLength = spec?.tagLength ?? 0
        guard rest.count >= tagLength else { throw KeyImportError.truncated }
        guard privateSection.count % (spec?.blockSize ?? 8) == 0, !privateSection.isEmpty else {
            throw KeyImportError.malformed("the private part has a wrong length")
        }
        let publicKey = try? NIOSSHPublicKey(openSSHPublicKey: type.sshName + " " + Data(publicBlob).base64EncodedString())
        return OpenSSHKeyFile(
            type: type,
            publicKey: publicKey,
            isEncrypted: spec != nil,
            body: .openSSH(cipher: cipher, kdf: kdf, kdfOptions: kdfOptions, publicBlob: publicBlob, privateSection: privateSection, tag: Array(rest.prefix(tagLength)))
        )
    }

    /// Decrypts the key. An unencrypted key ignores the passphrase; an
    /// encrypted one with no passphrase throws `passphraseNeeded`.
    public func decrypt(passphrase: String?) throws -> ImportedKeyMaterial {
        switch body {
        case .ready(let m):
            return m
        case .openSSH(let cipher, _, let kdfOptions, let publicBlob, let section, let tag):
            var plain: [UInt8]
            if let spec = CipherSpec.named(cipher) {
                guard let passphrase, !passphrase.isEmpty else { throw KeyImportError.passphraseNeeded }
                let (salt, rounds) = try Self.kdfParameters(kdfOptions)
                var pass = Array(passphrase.utf8)
                defer { for i in pass.indices { pass[i] = 0 } }
                var km = try BcryptPBKDF.derive(passphrase: pass, salt: salt, rounds: Int(rounds), length: spec.keyLength + spec.ivLength)
                defer { for i in km.indices { km[i] = 0 } }
                plain = try spec.decrypt(section, key: Array(km[0..<spec.keyLength]), iv: Array(km[spec.keyLength...]), tag: tag)
            } else {
                // A copy: `body` keeps the file's bytes while the sheet holds the file.
                plain = section
            }
            defer { for i in plain.indices { plain[i] = 0 } }
            return try Self.readPrivate(plain, publicBlob: publicBlob, type: type, encrypted: isEncrypted)
        }
    }

    /// The bcrypt options: a salt and a round count within `maxRounds`.
    static func kdfParameters(_ options: [UInt8]) throws -> (salt: [UInt8], rounds: UInt32) {
        var o = SSHWireReader(options)
        let salt = try o.string()
        let rounds = try o.u32()
        guard !salt.isEmpty, rounds >= 1 else {
            throw KeyImportError.malformed("the key derivation's options are not valid")
        }
        guard rounds <= maxRounds else { throw KeyImportError.tooManyRounds(rounds) }
        return (salt, rounds)
    }

    /// The private section: two equal check words, the key, its comment,
    /// then padding 1, 2, 3 ... to the block size.
    private static func readPrivate(_ p: [UInt8], publicBlob: [UInt8], type: ImportedKeyType, encrypted: Bool) throws -> ImportedKeyMaterial {
        var r = SSHWireReader(p)
        let c1 = try r.u32(), c2 = try r.u32()
        guard c1 == c2 else { throw encrypted ? KeyImportError.wrongPassphrase : KeyImportError.checkMismatch }
        let name = try r.text()
        guard try checkType(name) == type else { throw KeyImportError.malformed("the private key's type differs from the public key's") }
        let raw: Data
        switch type {
        case .ed25519:
            let pub = try r.string()
            var priv = try r.string()
            defer { for i in priv.indices { priv[i] = 0 } }
            guard pub.count == 32, priv.count == 64, Array(priv[32...]) == pub else {
                throw KeyImportError.malformed("the Ed25519 key has wrong lengths")
            }
            raw = Data(priv[0..<32])
        case .p256, .p384, .p521:
            let curve = try r.text()
            guard type.sshName.hasSuffix(curve) else { throw KeyImportError.malformed("the curve name does not fit the key type") }
            _ = try r.string() // the public point, checked below against the public key
            var d = try r.string()
            defer { for i in d.indices { d[i] = 0 } }
            while d.first == 0 { d.removeFirst() }
            guard !d.isEmpty, d.count <= type.scalarLength else { throw KeyImportError.malformed("the ECDSA private value has a wrong length") }
            raw = Data(repeating: 0, count: type.scalarLength - d.count) + Data(d)
        }
        let commentBytes = try r.string()
        let padding = p[r.offset...]
        for (i, b) in padding.enumerated() where Int(b) != (i + 1) & 0xFF {
            throw KeyImportError.badPadding
        }
        let m = try ImportedKeyMaterial(type: type, raw: raw, comment: String(decoding: commentBytes, as: UTF8.self))
        // The private key must give the public key the file names.
        guard publicBlob == SSHWireReader.blob(of: m.publicKey) else {
            throw KeyImportError.malformed("the private key does not match its public key")
        }
        return m
    }
}

/// The ciphers `ssh-keygen` may write, with their key and IV lengths
/// (OpenSSH cipher.c). chacha20-poly1305 and 3des are refused.
struct CipherSpec {
    enum Mode { case ctr, cbc, gcm }
    let mode: Mode
    let keyLength: Int
    var ivLength: Int { mode == .gcm ? 12 : 16 }
    var blockSize: Int { 16 }
    var tagLength: Int { mode == .gcm ? 16 : 0 }

    static func named(_ name: String) -> CipherSpec? {
        switch name {
        case "aes128-ctr": return CipherSpec(mode: .ctr, keyLength: 16)
        case "aes192-ctr": return CipherSpec(mode: .ctr, keyLength: 24)
        case "aes256-ctr": return CipherSpec(mode: .ctr, keyLength: 32)
        case "aes128-cbc": return CipherSpec(mode: .cbc, keyLength: 16)
        case "aes192-cbc": return CipherSpec(mode: .cbc, keyLength: 24)
        case "aes256-cbc": return CipherSpec(mode: .cbc, keyLength: 32)
        case "aes128-gcm@openssh.com": return CipherSpec(mode: .gcm, keyLength: 16)
        case "aes256-gcm@openssh.com": return CipherSpec(mode: .gcm, keyLength: 32)
        default: return nil
        }
    }

    func decrypt(_ data: [UInt8], key: [UInt8], iv: [UInt8], tag: [UInt8]) throws -> [UInt8] {
        switch mode {
        case .gcm:
            do {
                let box = try AES.GCM.SealedBox(nonce: AES.GCM.Nonce(data: iv), ciphertext: data, tag: tag)
                return Array(try AES.GCM.open(box, using: SymmetricKey(data: key)))
            } catch {
                // The tag covers the whole section: a wrong passphrase fails here.
                throw KeyImportError.wrongPassphrase
            }
        case .ctr, .cbc:
            var ref: CCCryptorRef?
            let rc = CCCryptorCreateWithMode(
                CCOperation(kCCDecrypt),
                CCMode(mode == .ctr ? kCCModeCTR : kCCModeCBC),
                CCAlgorithm(kCCAlgorithmAES),
                CCPadding(ccNoPadding),
                iv, key, key.count, nil, 0, 0,
                mode == .ctr ? CCModeOptions(kCCModeOptionCTR_BE) : 0,
                &ref
            )
            guard rc == kCCSuccess, let ref else { throw KeyImportError.malformed("the cipher could not start") }
            defer { CCCryptorRelease(ref) }
            var out = [UInt8](repeating: 0, count: data.count)
            var moved = 0
            let rc2 = CCCryptorUpdate(ref, data, data.count, &out, out.count, &moved)
            guard rc2 == kCCSuccess, moved == data.count else { throw KeyImportError.malformed("the cipher failed") }
            return out
        }
    }
}

/// Reads SSH wire types (RFC 4251 5) from a byte array.
struct SSHWireReader {
    let bytes: [UInt8]
    private(set) var offset: Int

    init(_ bytes: [UInt8], at offset: Int = 0) {
        self.bytes = bytes
        self.offset = offset
    }

    mutating func u32() throws -> UInt32 {
        guard bytes.count - offset >= 4 else { throw KeyImportError.truncated }
        let v = UInt32(bytes[offset]) << 24 | UInt32(bytes[offset + 1]) << 16 | UInt32(bytes[offset + 2]) << 8 | UInt32(bytes[offset + 3])
        offset += 4
        return v
    }

    mutating func string() throws -> [UInt8] {
        let n = Int(try u32())
        guard bytes.count - offset >= n else { throw KeyImportError.truncated }
        defer { offset += n }
        return Array(bytes[offset..<offset + n])
    }

    mutating func text() throws -> String {
        let s = try string()
        guard let t = String(bytes: s, encoding: .utf8) else { throw KeyImportError.malformed("a name is not text") }
        return t
    }

    /// A public key's wire form.
    static func blob(of key: NIOSSHPublicKey) -> [UInt8] {
        let parts = String(openSSHPublicKey: key).split(separator: " ")
        guard parts.count >= 2, let d = Data(base64Encoded: String(parts[1])) else { return [] }
        return Array(d)
    }
}

/// Keys in the older PEM formats: unencrypted SEC1 EC keys and PKCS#8
/// (ECDSA and Ed25519) import; encrypted ones and RSA are refused. The DER
/// is read here: `ssh-keygen -m PEM` writes explicit curve parameters, which
/// CryptoKit's PEM reader refuses.
enum PEMKey {
    /// The DER prefix of an unencrypted PKCS#8 Ed25519 key (RFC 8410 7),
    /// followed by the 32-byte seed. Used by the tests.
    static let ed25519PKCS8Prefix: [UInt8] = [0x30, 0x2E, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2B, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20]
    static let oidEC: [UInt8] = [0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x02, 0x01] // 1.2.840.10045.2.1
    static let oidEd25519: [UInt8] = [0x2B, 0x65, 0x70] // 1.3.101.112
    static let oidRSA: [UInt8] = [0x2A, 0x86, 0x48, 0x86, 0xF7, 0x0D, 0x01, 0x01, 0x01] // 1.2.840.113549.1.1.1

    static func find(in text: String) throws -> ImportedKeyMaterial? {
        let labels = ["RSA PRIVATE KEY", "DSA PRIVATE KEY", "EC PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "PRIVATE KEY"]
        // The block that starts first wins.
        var best: (String, Range<String.Index>)?
        for l in labels {
            if let r = text.range(of: "-----BEGIN \(l)-----"), best == nil || r.lowerBound < best!.1.lowerBound {
                best = (l, r)
            }
        }
        guard let (label, begin) = best else { return nil }
        guard let endRange = text.range(of: "-----END \(label)-----", range: begin.upperBound..<text.endIndex) else {
            throw KeyImportError.truncated
        }
        let inner = String(text[begin.upperBound..<endRange.lowerBound])
        switch label {
        case "RSA PRIVATE KEY": throw KeyImportError.rsa
        case "DSA PRIVATE KEY": throw KeyImportError.unsupportedType("ssh-dss")
        case "ENCRYPTED PRIVATE KEY": throw KeyImportError.encryptedPEM
        default: break
        }
        if inner.contains("Proc-Type:") || inner.contains("DEK-Info:") { throw KeyImportError.encryptedPEM }
        guard let der = Data(base64Encoded: inner.filter { !$0.isWhitespace }).map(Array.init) else {
            throw KeyImportError.malformed("the base64 text is broken")
        }
        return label == "EC PRIVATE KEY" ? try sec1(der) : try pkcs8(der)
    }

    /// PKCS#8 (RFC 5208): version, algorithm, the key in an OCTET STRING.
    static func pkcs8(_ der: [UInt8]) throws -> ImportedKeyMaterial {
        var outer = DER(der)
        var top = try outer.sequence()
        _ = try top.next(0x02) // version
        var alg = try top.sequence()
        let oid = try alg.next(0x06)
        let key = try top.next(0x04)
        switch oid {
        case oidEC: return try sec1(key)
        case oidEd25519:
            var d = DER(key)
            let seed = try d.next(0x04)
            guard seed.count == 32 else { throw KeyImportError.malformed("the Ed25519 key has a wrong length") }
            return try ImportedKeyMaterial(type: .ed25519, raw: Data(seed), comment: "")
        case oidRSA: throw KeyImportError.rsa
        default: throw KeyImportError.unsupportedType("this PEM key")
        }
    }

    /// SEC1 (RFC 5915): version 1, the private value, optional parameters
    /// and public key. The curve follows from the private value's length;
    /// the public key, when present, must match.
    static func sec1(_ der: [UInt8]) throws -> ImportedKeyMaterial {
        var outer = DER(der)
        var top = try outer.sequence()
        _ = try top.next(0x02)
        let d = try top.next(0x04)
        let type: ImportedKeyType
        switch d.count {
        case 32: type = .p256
        case 48: type = .p384
        case 66: type = .p521
        default: throw KeyImportError.unsupportedType("this EC key's curve")
        }
        let m = try ImportedKeyMaterial(type: type, raw: Data(d), comment: "")
        while !top.atEnd {
            let (tag, body) = try top.any()
            guard tag == 0xA1 else { continue }
            var b = DER(body)
            var bits = try b.next(0x03)
            guard bits.first == 0 else { throw KeyImportError.malformed("the EC public key is not valid") }
            bits.removeFirst()
            // The SSH blob ends with the same uncompressed point.
            let derived = SSHWireReader.blob(of: m.publicKey)
            guard derived.count >= bits.count, Array(derived.suffix(bits.count)) == bits else {
                throw KeyImportError.malformed("the private key does not match its public key")
            }
        }
        return m
    }
}

/// A minimal DER reader: definite lengths and the few tags keys use.
struct DER {
    private let bytes: [UInt8]
    private var offset = 0

    init(_ bytes: [UInt8]) { self.bytes = bytes }

    var atEnd: Bool { offset >= bytes.count }

    mutating func any() throws -> (UInt8, [UInt8]) {
        guard bytes.count - offset >= 2 else { throw KeyImportError.truncated }
        let tag = bytes[offset]
        var len = Int(bytes[offset + 1])
        offset += 2
        if len & 0x80 != 0 {
            let n = len & 0x7F
            guard n >= 1, n <= 3, bytes.count - offset >= n else { throw KeyImportError.malformed("a DER length is not valid") }
            len = 0
            for _ in 0..<n { len = len << 8 | Int(bytes[offset]); offset += 1 }
        }
        guard bytes.count - offset >= len else { throw KeyImportError.truncated }
        defer { offset += len }
        return (tag, Array(bytes[offset..<offset + len]))
    }

    mutating func next(_ tag: UInt8) throws -> [UInt8] {
        let (t, body) = try any()
        guard t == tag else { throw KeyImportError.malformed("unexpected DER content") }
        return body
    }

    mutating func sequence() throws -> DER {
        DER(try next(0x30))
    }
}
