import Foundation
import HabAPI

/// One frame on a data connection (hab-logic-api.md 5.3): a kind byte, a
/// 4-byte big-endian payload length, then the payload.
public struct DataFrame: Sendable, Equatable {
    public var kind: UInt8
    public var payload: Data

    public init(kind: UInt8, payload: Data) {
        self.kind = kind
        self.payload = payload
    }

    /// Payloads are at most 64 MiB (hab-logic-api.md 5.8).
    public static let maxPayload = 64 << 20

    public func encoded() -> Data {
        var d = Data(capacity: 5 + payload.count)
        d.append(kind)
        let n = UInt32(payload.count)
        d.append(contentsOf: [UInt8(n >> 24), UInt8(n >> 16 & 0xFF), UInt8(n >> 8 & 0xFF), UInt8(n & 0xFF)])
        d.append(payload)
        return d
    }

    public static func json<T: Encodable>(_ kind: FrameKind, _ value: T) throws -> DataFrame {
        DataFrame(kind: kind.rawValue, payload: try JSONEncoder().encode(value))
    }

    public func decode<T: Decodable>(_ type: T.Type) throws -> T {
        try JSONDecoder().decode(T.self, from: payload)
    }

    /// A snapshot or bytes payload: the 8-byte offset, then the rest. A
    /// snapshot also carries its encoding name.
    func offsetPayload(named: Bool) throws -> (offset: UInt64, encoding: String, data: Data) {
        let p = payload
        guard p.count >= 8 else { throw ProtocolError("short frame payload") }
        var off: UInt64 = 0
        var i = p.startIndex
        for _ in 0..<8 {
            off = off << 8 | UInt64(p[i])
            i = p.index(after: i)
        }
        var enc = ""
        if named {
            guard i < p.endIndex else { throw ProtocolError("snapshot without an encoding") }
            let n = Int(p[i])
            i = p.index(after: i)
            guard p.distance(from: i, to: p.endIndex) >= n else { throw ProtocolError("short snapshot encoding") }
            let end = p.index(i, offsetBy: n)
            enc = String(decoding: p[i..<end], as: UTF8.self)
            i = end
        }
        return (off, enc, Data(p[i...]))
    }

    /// Builds a bytes frame (the fake hab and tests use it).
    public static func bytes(offset: UInt64, _ data: Data) -> DataFrame {
        DataFrame(kind: FrameKind.bytes.rawValue, payload: offsetBytes(offset) + data)
    }

    /// Builds a snapshot frame (the fake hab and tests use it).
    public static func snapshot(offset: UInt64, encoding: String = "snapshot-vt", _ data: Data) -> DataFrame {
        var p = offsetBytes(offset)
        let name = Data(encoding.utf8)
        p.append(UInt8(name.count))
        p.append(name)
        p.append(data)
        return DataFrame(kind: FrameKind.snapshot.rawValue, payload: p)
    }

    private static func offsetBytes(_ v: UInt64) -> Data {
        Data((0..<8).map { UInt8(v >> (56 - 8 * $0) & 0xFF) })
    }
}

/// Splits a byte stream into frames.
public struct FrameBuffer: Sendable {
    private var pending = Data()

    public init() {}

    public mutating func append(_ chunk: Data) throws -> [DataFrame] {
        pending.append(chunk)
        var out: [DataFrame] = []
        var start = pending.startIndex
        while pending.distance(from: start, to: pending.endIndex) >= 5 {
            let b = pending[start...].prefix(5).map { $0 }
            let n = Int(b[1]) << 24 | Int(b[2]) << 16 | Int(b[3]) << 8 | Int(b[4])
            if n > DataFrame.maxPayload { throw ProtocolError("a frame is longer than \(DataFrame.maxPayload) bytes") }
            let body = pending.index(start, offsetBy: 5)
            guard pending.distance(from: body, to: pending.endIndex) >= n else { break }
            let end = pending.index(body, offsetBy: n)
            out.append(DataFrame(kind: b[0], payload: Data(pending[body..<end])))
            start = end
        }
        pending = Data(pending[start...])
        return out
    }
}
