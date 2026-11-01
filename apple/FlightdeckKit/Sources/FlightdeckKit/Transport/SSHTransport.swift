#if os(macOS)
import Darwin
import Foundation

/// A remote hab through the system ssh and the user's own keys:
/// `ssh <host> gff-hab connect` (hab-logic-api.md 4.2). Each `open` starts
/// one ssh process; its stdin and stdout are the stream.
///
/// ssh never prompts (BatchMode): a host it cannot reach with a key fails, and
/// its last stderr lines become the stream's diagnostics. Connection sharing
/// (one SSH connection for the control stream and every data stream) is left
/// for the data stream stage; today each stream is its own ssh.
public struct SSHTransport: Transport {
    /// The ssh destination: an alias from the user's ssh config, a name, or user@name.
    public var host: String
    /// The command run on the host. Its words go to the remote shell as they are.
    public var remoteCommand: [String]
    /// Extra ssh options, placed before the transport's own. OpenSSH keeps the
    /// first value it reads, so these override the defaults.
    public var options: [String]
    /// The ssh program.
    public var sshPath: String

    public init(host: String, remoteCommand: [String] = ["gff-hab", "connect"], options: [String] = [], sshPath: String = "/usr/bin/ssh") {
        self.host = host
        self.remoteCommand = remoteCommand.isEmpty ? ["gff-hab", "connect"] : remoteCommand
        self.options = options
        self.sshPath = sshPath
    }

    public var endpoint: String { "ssh " + host }

    /// The ssh arguments for one stream, without the program.
    public func arguments() throws -> [String] {
        if host.isEmpty || host.hasPrefix("-") || host.contains(where: { $0.isWhitespace }) {
            throw TransportError("ssh host \"\(host)\" is not a destination")
        }
        return options + [
            "-T",
            "-o", "BatchMode=yes",
            "-o", "ConnectTimeout=10",
            "-o", "ServerAliveInterval=15",
            "-o", "ServerAliveCountMax=3",
            "--", host,
        ] + remoteCommand
    }

    public func open(_ kind: StreamKind) async throws -> ByteStream {
        let args = try arguments()
        var toChild: [Int32] = [0, 0]
        var fromChild: [Int32] = [0, 0]
        var errPipe: [Int32] = [0, 0]
        guard pipe(&toChild) == 0 else { throw TransportError("pipe: " + String(cString: strerror(errno))) }
        guard pipe(&fromChild) == 0 else {
            Darwin.close(toChild[0]); Darwin.close(toChild[1])
            throw TransportError("pipe: " + String(cString: strerror(errno)))
        }
        guard pipe(&errPipe) == 0 else {
            for fd in toChild + fromChild { Darwin.close(fd) }
            throw TransportError("pipe: " + String(cString: strerror(errno)))
        }
        // Our ends stay out of every other child.
        prepareDescriptor(toChild[1], noSigPipe: true)
        prepareDescriptor(fromChild[0], noSigPipe: false)
        prepareDescriptor(errPipe[0], noSigPipe: false)

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: sshPath)
        proc.arguments = args
        let childIn = FileHandle(fileDescriptor: toChild[0], closeOnDealloc: true)
        let childOut = FileHandle(fileDescriptor: fromChild[1], closeOnDealloc: true)
        let childErr = FileHandle(fileDescriptor: errPipe[1], closeOnDealloc: true)
        proc.standardInput = childIn
        proc.standardOutput = childOut
        proc.standardError = childErr
        do {
            try proc.run()
        } catch {
            for fd in [toChild[1], fromChild[0], errPipe[0]] { Darwin.close(fd) }
            try? childIn.close(); try? childOut.close(); try? childErr.close()
            throw TransportError("start \(sshPath): \(error.localizedDescription)")
        }
        try? childIn.close()
        try? childOut.close()
        try? childErr.close()

        let tail = StderrTail(fd: errPipe[0])
        let box = ProcessBox(proc)
        return FDStream(
            read: fromChild[0],
            write: toChild[1],
            onClose: { box.stop() },
            diagnostics: { tail.text }
        )
    }
}

/// Keeps the last few lines a child printed on stderr.
final class StderrTail: @unchecked Sendable {
    private let lock = NSLock()
    private var buf = Data()
    private let limit = 2048

    init(fd: Int32) {
        let t = Thread { [self] in
            var chunk = [UInt8](repeating: 0, count: 1024)
            while true {
                let n = chunk.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress, $0.count) }
                if n < 0 && errno == EINTR { continue }
                if n <= 0 { break }
                lock.lock()
                buf.append(contentsOf: chunk[0..<n])
                if buf.count > limit { buf = buf.suffix(limit) }
                lock.unlock()
            }
            Darwin.close(fd)
        }
        t.name = "flightdeck.ssh.stderr"
        t.start()
    }

    var text: String? {
        lock.lock(); defer { lock.unlock() }
        let s = String(decoding: buf, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        return s.isEmpty ? nil : s
    }
}

/// Stops an ssh child: SIGTERM now, SIGKILL after two seconds if it lingers.
final class ProcessBox: @unchecked Sendable {
    private let proc: Process
    init(_ proc: Process) { self.proc = proc }

    func stop() {
        guard proc.isRunning else { return }
        proc.terminate()
        let pid = proc.processIdentifier
        let p = proc
        DispatchQueue.global().asyncAfter(deadline: .now() + 2) {
            if p.isRunning { kill(pid, SIGKILL) }
        }
    }
}
#endif
