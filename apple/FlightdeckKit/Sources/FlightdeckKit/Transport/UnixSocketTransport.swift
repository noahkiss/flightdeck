import Darwin
import Foundation

/// The hab on this machine, over its Unix socket (hab-logic-api.md 4.1).
/// Each `open` is a new connection to the same socket; the hab tells control
/// from data by the first byte.
public struct UnixSocketTransport: Transport {
    public let path: String

    public init(path: String) {
        self.path = path
    }

    public var endpoint: String { "local" }

    /// The socket path gff-hab uses by default: `$GFF_HAB_SOCKET`, else
    /// `gff/hab.sock` under the user config directory on macOS (Application
    /// Support) and under `$XDG_RUNTIME_DIR` elsewhere. Nil when no rule applies.
    public static func defaultPath(environment env: [String: String] = ProcessInfo.processInfo.environment) -> String? {
        if let p = env["GFF_HAB_SOCKET"], !p.isEmpty {
            return p
        }
        #if os(macOS)
        guard let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first else {
            return nil
        }
        return dir.appendingPathComponent("gff/hab.sock").path
        #else
        if let rt = env["XDG_RUNTIME_DIR"], rt.hasPrefix("/") {
            return rt + "/gff/hab.sock"
        }
        return nil
        #endif
    }

    public func open(_ kind: StreamKind) async throws -> ByteStream {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 {
            throw TransportError("socket: " + String(cString: strerror(errno)))
        }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        let cap = MemoryLayout.size(ofValue: addr.sun_path)
        guard bytes.count < cap else {
            Darwin.close(fd)
            throw TransportError("socket path is \(bytes.count) bytes; the limit is \(cap - 1): \(path)")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
            raw[bytes.count] = 0
        }
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        prepareDescriptor(fd, noSigPipe: false)
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if rc != 0 {
            let e = errno
            Darwin.close(fd)
            throw TransportError("connect \(path): " + String(cString: strerror(e)))
        }
        return FDStream(socket: fd)
    }
}
