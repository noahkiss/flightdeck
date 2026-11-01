import Darwin
import Foundation

/// A byte stream over file descriptors: one socket, or a pair of pipes to a
/// child process. Both Mac carriers use it.
///
/// A thread does blocking reads and queues what it reads. Writes run on a
/// serial queue. The descriptors close on that queue once the reader has
/// stopped, so a write in flight never touches a reused descriptor.
final class FDStream: ByteStream, @unchecked Sendable {
    private let readFD: Int32
    private let writeFD: Int32
    private let isSocket: Bool
    private let onClose: (@Sendable () -> Void)?
    private let diagnose: (@Sendable () -> String?)?

    private let lock = NSLock()
    private var chunks: [Data] = []
    private var finished = false
    private var failure: Error?
    private var waiter: CheckedContinuation<Data?, Error>?
    private var closing = false
    private var readerDone = false

    private let writes = DispatchQueue(label: "flightdeck.fdstream.write")
    /// Touched only on `writes`.
    private var writeClosed = false

    /// A stream over one connected socket.
    convenience init(socket fd: Int32) {
        self.init(readFD: fd, writeFD: fd, isSocket: true, onClose: nil, diagnostics: nil)
    }

    /// A stream over a pair of pipes. `onClose` runs once when the stream is
    /// closed by this side, for example to stop the child process.
    convenience init(read: Int32, write: Int32, onClose: (@Sendable () -> Void)?, diagnostics: (@Sendable () -> String?)?) {
        self.init(readFD: read, writeFD: write, isSocket: false, onClose: onClose, diagnostics: diagnostics)
    }

    private init(readFD: Int32, writeFD: Int32, isSocket: Bool, onClose: (@Sendable () -> Void)?, diagnostics: (@Sendable () -> String?)?) {
        self.readFD = readFD
        self.writeFD = writeFD
        self.isSocket = isSocket
        self.onClose = onClose
        self.diagnose = diagnostics
        OpenStreams.opened()
        let thread = Thread { [self] in readLoop() }
        thread.name = "flightdeck.fdstream.read"
        thread.start()
    }

    var diagnostics: String? { diagnose?() }

    func read() async throws -> Data? {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data?, Error>) in
            lock.lock()
            if !chunks.isEmpty {
                let d = chunks.removeFirst()
                lock.unlock()
                c.resume(returning: d)
            } else if finished {
                let f = failure
                lock.unlock()
                if let f { c.resume(throwing: f) } else { c.resume(returning: nil) }
            } else {
                waiter = c
                lock.unlock()
            }
        }
    }

    func write(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            writes.async { [self] in
                if writeClosed {
                    c.resume(throwing: TransportError("stream closed"))
                    return
                }
                var err: Int32 = 0
                data.withUnsafeBytes { raw in
                    var off = 0
                    while off < raw.count {
                        let n = Darwin.write(writeFD, raw.baseAddress! + off, raw.count - off)
                        if n > 0 {
                            off += n
                        } else if n < 0 && errno == EINTR {
                            continue
                        } else {
                            err = n < 0 ? errno : EPIPE
                            return
                        }
                    }
                }
                if err != 0 {
                    c.resume(throwing: TransportError("write: " + String(cString: strerror(err))))
                } else {
                    c.resume()
                }
            }
        }
    }

    func close() {
        lock.lock()
        if closing {
            lock.unlock()
            return
        }
        closing = true
        if isSocket && !readerDone {
            // Wakes the blocked read with end of file. The descriptor stays valid:
            // the reader closes it only after it marks itself done under this lock.
            Darwin.shutdown(readFD, SHUT_RDWR)
        }
        let w = waiter
        waiter = nil
        finished = true
        failure = nil
        chunks.removeAll()
        lock.unlock()
        w?.resume(returning: nil)
        if !isSocket {
            writes.async { [self] in
                if !writeClosed {
                    Darwin.close(writeFD)
                    writeClosed = true
                }
            }
        }
        onClose?()
    }

    private func readLoop() {
        var buf = [UInt8](repeating: 0, count: 64 * 1024)
        var end: Error?
        while true {
            let n = buf.withUnsafeMutableBytes { Darwin.read(readFD, $0.baseAddress, $0.count) }
            if n > 0 {
                deliver(Data(buf[0..<n]))
                continue
            }
            if n < 0 && errno == EINTR { continue }
            if n < 0 { end = TransportError("read: " + String(cString: strerror(errno))) }
            break
        }
        lock.lock()
        readerDone = true
        let w = finished ? nil : waiter
        if !finished {
            finished = true
            failure = end
            waiter = nil
        }
        lock.unlock()
        if let w {
            if let end { w.resume(throwing: end) } else { w.resume(returning: nil) }
        }
        writes.async { [self] in
            if !writeClosed {
                if !isSocket { Darwin.close(writeFD) }
                writeClosed = true
            }
            Darwin.close(readFD)
            OpenStreams.closed()
        }
    }

    private func deliver(_ d: Data) {
        lock.lock()
        if finished {
            lock.unlock()
            return
        }
        if let w = waiter {
            waiter = nil
            lock.unlock()
            w.resume(returning: d)
        } else {
            chunks.append(d)
            lock.unlock()
        }
    }
}

/// Sets close-on-exec and, where it applies, no SIGPIPE on a descriptor.
func prepareDescriptor(_ fd: Int32, noSigPipe: Bool) {
    _ = fcntl(fd, F_SETFD, FD_CLOEXEC)
    if noSigPipe {
        _ = fcntl(fd, F_SETNOSIGPIPE, 1)
    }
}
