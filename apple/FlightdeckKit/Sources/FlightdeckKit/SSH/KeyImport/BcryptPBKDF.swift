import CryptoKit
import Foundation

/// Blowfish with the "expensive key schedule" bcrypt adds: only what
/// `bcrypt_pbkdf` needs (Schneier 1993; Provos and Mazières 1999).
///
/// The state is one buffer of 1042 words: the P-array (18), then the four
/// S-boxes (256 each). Raw pointers keep the inner loop fast enough in debug
/// builds too, where a 16-round key file runs about 2 million block
/// encryptions.
final class Blowfish {
    private static let words = 18 + 4 * 256
    private static let initial: [UInt32] = BlowfishTables.p + BlowfishTables.s0 + BlowfishTables.s1 + BlowfishTables.s2 + BlowfishTables.s3
    private let state: UnsafeMutablePointer<UInt32>

    /// The initial state, before any key.
    init() {
        state = .allocate(capacity: Self.words)
        Self.initial.withUnsafeBufferPointer { state.initialize(from: $0.baseAddress!, count: Self.words) }
    }

    /// The plain key schedule, for the published Blowfish test vectors.
    convenience init(key: [UInt8]) {
        self.init()
        expand(key: key, salt: nil)
    }

    deinit {
        state.update(repeating: 0, count: Self.words)
        state.deallocate()
    }

    /// Encrypts one 64-bit block held as two big-endian halves.
    @inline(__always)
    func encrypt(_ l: inout UInt32, _ r: inout UInt32) {
        let p = state
        let s0 = state + 18, s1 = state + 274, s2 = state + 530, s3 = state + 786
        var xl = l, xr = r
        var i = 0
        while i < 16 {
            xl ^= p[i]
            xr ^= ((s0[Int(xl >> 24)] &+ s1[Int((xl >> 16) & 0xFF)]) ^ s2[Int((xl >> 8) & 0xFF)]) &+ s3[Int(xl & 0xFF)]
            xr ^= p[i + 1]
            xl ^= ((s0[Int(xr >> 24)] &+ s1[Int((xr >> 16) & 0xFF)]) ^ s2[Int((xr >> 8) & 0xFF)]) &+ s3[Int(xr & 0xFF)]
            i += 2
        }
        l = xr ^ p[17]
        r = xl ^ p[16]
    }

    /// The next 32 bits of `data`, big-endian, wrapping round at its end.
    @inline(__always)
    private static func word(_ data: UnsafeBufferPointer<UInt8>, _ j: inout Int) -> UInt32 {
        var w: UInt32 = 0
        for _ in 0..<4 {
            w = (w << 8) | UInt32(data[j])
            j += 1
            if j == data.count { j = 0 }
        }
        return w
    }

    /// The key schedule. With `salt`, each block is mixed with the salt's
    /// words before encryption (bcrypt's EksBlowfish expand); without it this
    /// is Blowfish's own schedule.
    func expand(key: [UInt8], salt: [UInt8]?) {
        key.withUnsafeBufferPointer { k in
            var j = 0
            for i in 0..<18 { state[i] ^= Self.word(k, &j) }
        }
        var l: UInt32 = 0, r: UInt32 = 0
        if let salt {
            salt.withUnsafeBufferPointer { d in
                var j = 0
                var i = 0
                while i < Self.words {
                    l ^= Self.word(d, &j)
                    r ^= Self.word(d, &j)
                    encrypt(&l, &r)
                    state[i] = l
                    state[i + 1] = r
                    i += 2
                }
            }
        } else {
            var i = 0
            while i < Self.words {
                encrypt(&l, &r)
                state[i] = l
                state[i + 1] = r
                i += 2
            }
        }
    }
}

/// OpenBSD's `bcrypt_pbkdf`, the key derivation in OpenSSH's private key
/// format. A PBKDF2-like construction over a bcrypt-style hash: SHA-512
/// turns the passphrase and each salt into 64 bytes, a 64-round
/// EksBlowfish setup encrypts a fixed 32-byte text, and output bytes are
/// spread across the key so every byte needs every round.
enum BcryptPBKDF {
    /// The fixed text the inner hash encrypts.
    private static let magic = Array("OxychromaticBlowfishSwatDynamite".utf8)

    enum Failure: Error, Equatable {
        case badParameters
    }

    /// Derives `length` bytes (1 to 1024) from the passphrase and salt.
    static func derive(passphrase: [UInt8], salt: [UInt8], rounds: Int, length: Int) throws -> [UInt8] {
        guard rounds >= 1, length >= 1, length <= 1024, !salt.isEmpty, salt.count <= 1 << 20 else {
            throw Failure.badParameters
        }
        let stride = (length + 31) / 32
        var amount = (length + stride - 1) / stride
        let passHash = Array(SHA512.hash(data: passphrase))
        var key = [UInt8](repeating: 0, count: length)
        var left = length
        var count: UInt32 = 1
        while left > 0 {
            var countSalt = salt
            countSalt.append(contentsOf: [UInt8(count >> 24), UInt8((count >> 16) & 0xFF), UInt8((count >> 8) & 0xFF), UInt8(count & 0xFF)])
            var tmp = hash(pass: passHash, salt: Array(SHA512.hash(data: countSalt)))
            var out = tmp
            if rounds > 1 {
                for _ in 1..<rounds {
                    tmp = hash(pass: passHash, salt: Array(SHA512.hash(data: tmp)))
                    for k in 0..<out.count { out[k] ^= tmp[k] }
                }
            }
            amount = min(amount, left)
            var i = 0
            while i < amount {
                let dest = i * stride + Int(count - 1)
                if dest >= length { break }
                key[dest] = out[i]
                i += 1
            }
            left -= i
            count += 1
        }
        return key
    }

    /// The inner hash: 32 bytes from two 64-byte SHA-512 digests.
    static func hash(pass: [UInt8], salt: [UInt8]) -> [UInt8] {
        let bf = Blowfish()
        bf.expand(key: pass, salt: salt)
        for _ in 0..<64 {
            bf.expand(key: salt, salt: nil)
            bf.expand(key: pass, salt: nil)
        }
        var words = [UInt32](repeating: 0, count: 8)
        for i in 0..<8 {
            words[i] = UInt32(magic[4 * i]) << 24 | UInt32(magic[4 * i + 1]) << 16 | UInt32(magic[4 * i + 2]) << 8 | UInt32(magic[4 * i + 3])
        }
        for _ in 0..<64 {
            var i = 0
            while i < 8 {
                var l = words[i], r = words[i + 1]
                bf.encrypt(&l, &r)
                words[i] = l
                words[i + 1] = r
                i += 2
            }
        }
        // Each word goes out little-endian, as OpenBSD writes it.
        var out = [UInt8](repeating: 0, count: 32)
        for i in 0..<8 {
            out[4 * i] = UInt8(words[i] & 0xFF)
            out[4 * i + 1] = UInt8((words[i] >> 8) & 0xFF)
            out[4 * i + 2] = UInt8((words[i] >> 16) & 0xFF)
            out[4 * i + 3] = UInt8(words[i] >> 24)
        }
        return out
    }
}
