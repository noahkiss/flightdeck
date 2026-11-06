import Foundation
import NIOCore
import NIOSSH

/// The hidden password setup (transport.md 6.1.6), like `ssh-copy-id`: for
/// a host that cannot run the pairing window. The app signs in once with the
/// account password, over a host key the owner already trusted, appends its
/// key line to `~/.ssh/authorized_keys` with the rules of 6.1.4, and drops
/// the password. It never stores the password and never uses password auth
/// again.
public enum PasswordSetup {
    /// What the host's file held.
    public enum Outcome: Equatable, Sendable {
        /// The line was appended.
        case added
        /// The key was already in the file; nothing was written.
        case present
    }

    /// The longest output the setup reads back.
    static let maxOutput = 8 * 1024

    /// The forced command, checked: it sits inside `command="..."` and the
    /// shell script's single quotes, so quotes, backslashes and control
    /// characters are refused.
    public static func forcedCommand(_ command: String?) throws -> String {
        let c = (command ?? "").trimmingCharacters(in: .whitespaces)
        let cmd = c.isEmpty ? NIOSSHTransport.defaultCommand : c
        guard cmd.count <= 512,
              cmd.unicodeScalars.allSatisfy({ !CharacterSet.controlCharacters.contains($0) && $0 != "\"" && $0 != "'" && $0 != "\\" && $0.properties.generalCategory != .format })
        else {
            throw TransportError("the command on the host cannot hold quotes, backslashes or control characters")
        }
        return cmd
    }

    /// The `authorized_keys` line (transport.md 6.1.4): `restrict`, the
    /// forced command, the key, and a `flightdeck:` comment naming the
    /// device and the day.
    public static func keyLine(publicKey: NIOSSHPublicKey, command: String, name: String, date: Date = Date()) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "UTC")
        f.dateFormat = "yyyy-MM-dd"
        // The comment sits inside single quotes in the script: no quote may reach it.
        let shown = Pairing.cleanName(name).replacingOccurrences(of: "'", with: "")
        return "restrict,command=\"\(command)\" \(String(openSSHPublicKey: publicKey)) flightdeck: \(shown) (paired \(f.string(from: date)))"
    }

    /// The POSIX shell script `sh -s` reads on the host. It appends the line
    /// with `>>`, so other lines are never changed or reordered and a link is
    /// followed; it adds a newline first when the last line had none, makes a
    /// missing `~/.ssh` at 0700, and leaves the file at 0600. A key already in
    /// the file is left alone.
    static func script(line: String, key: NIOSSHPublicKey) -> String {
        // Quoted for sh whatever the caller passes: ' becomes '\'' and a line
        // break cannot reach the file as a second line.
        func q(_ s: String) -> String {
            "'" + s.filter { !$0.isNewline && $0 != "\0" }.replacingOccurrences(of: "'", with: "'\\''") + "'"
        }
        let k = q(String(openSSHPublicKey: key))
        let l = q(line)
        return """
        umask 077
        d="$HOME/.ssh"
        f="$d/authorized_keys"
        key=\(k)
        line=\(l)
        if [ ! -d "$d" ]; then mkdir -m 700 "$d" || { echo 'flightdeck: error could not make ~/.ssh'; exit 3; }; fi
        if [ -f "$f" ] && grep -v '^[[:space:]]*#' "$f" | grep -qF -- "$key"; then echo 'flightdeck: present'; exit 0; fi
        if [ -s "$f" ] && [ -n "$(tail -c 1 "$f")" ]; then printf '\\n' >> "$f" || { echo 'flightdeck: error could not write the file'; exit 3; }; fi
        printf '%s\\n' "$line" >> "$f" || { echo 'flightdeck: error could not write the file'; exit 3; }
        chmod 600 "$f"
        echo 'flightdeck: added'
        exit 0

        """
    }

    /// Reads the script's answer from its output.
    static func outcome(_ output: Data) throws -> Outcome {
        let text = String(decoding: output.prefix(maxOutput), as: UTF8.self)
        for l in text.split(whereSeparator: \.isNewline) {
            switch l {
            case "flightdeck: added": return .added
            case "flightdeck: present": return .present
            default:
                if l.hasPrefix("flightdeck: error ") {
                    throw TransportError("the host could not add the key: " + l.dropFirst("flightdeck: error ".count))
                }
            }
        }
        throw TransportError("the host did not run the setup; it needs a POSIX sh, grep and tail")
    }

    /// Signs in with the password over the pinned host key and runs the
    /// script. The password lives only in this call.
    public static func install(
        _ dest: SSHDestination,
        hostKeyFingerprint: String,
        password: String,
        line: String,
        key: NIOSSHPublicKey,
        timing: SSHTiming = SSHTiming()
    ) async throws -> Outcome {
        guard SSHFingerprint.isValid(hostKeyFingerprint) else { throw TransportError("no trusted host key") }
        let conn = try await SSHConnection.dial(
            dest.endpoint, user: dest.user,
            auth: ClientPasswordAuth(user: dest.user, password: password),
            check: .pinned(hostKeyFingerprint), timing: timing
        )
        defer { conn.close() }
        // One bound over the whole exchange: a host that never answers the
        // exec request, or never ends, is closed.
        let timer = Task {
            try await Task.sleep(for: .seconds(20))
            conn.close()
        }
        defer { timer.cancel() }
        return try await withTaskCancellationHandler {
            let stream = try await conn.openSession(.exec("sh -s"), onEnd: {}, onFirstData: nil)
            defer { stream.close() }
            try await stream.write(Data(script(line: line, key: key).utf8))
            await stream.closeWrite()
            var out = Data()
            while out.count < maxOutput, let d = try await stream.read() {
                out.append(d)
            }
            return try outcome(out)
        } onCancel: {
            conn.close()
        }
    }
}

/// Offers the password once. A host with password auth off, or one that
/// refuses the password, ends the dial with a plain reason.
final class ClientPasswordAuth: NIOSSHClientUserAuthenticationDelegate, @unchecked Sendable {
    let user: String
    private var password: String?

    init(user: String, password: String) {
        self.user = user
        self.password = password
    }

    func nextAuthenticationType(availableMethods: NIOSSHAvailableUserAuthenticationMethods, nextChallengePromise: EventLoopPromise<NIOSSHUserAuthenticationOffer?>) {
        guard availableMethods.contains(.password) else {
            password = nil
            nextChallengePromise.fail(PasswordAuthOff(user: user))
            return
        }
        guard let p = password else {
            nextChallengePromise.fail(PasswordRefused(user: user))
            return
        }
        // Offered once, then dropped.
        password = nil
        nextChallengePromise.succeed(.init(username: user, serviceName: "", offer: .password(.init(password: p))))
    }
}

/// The host does not take passwords.
public struct PasswordAuthOff: Error, CustomStringConvertible, Sendable {
    public var user: String
    public var description: String {
        "the host does not take a password for \(user). Pair with gff-hab pair, or add the key by hand"
    }
}

/// The host refused the password.
public struct PasswordRefused: Error, CustomStringConvertible, Sendable {
    public var user: String
    public var description: String { "the host refused the password for \(user)" }
}
