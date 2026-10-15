import Foundation

/// Holds an advisory lock for the lifetime of one helper process.
///
/// The command socket cannot provide exclusivity by itself: unlinking a live
/// Unix-domain socket only removes its name, leaving the old listener running
/// and unreachable. A separate file whose inode is never replaced prevents a
/// second helper from stealing that name during overlapping app launches.
final class HelperInstanceLock {
    private var fileDescriptor: Int32 = -1

    init(path: String) throws {
        let descriptor = open(path, O_CREAT | O_RDWR, S_IRUSR | S_IWUSR)
        guard descriptor >= 0 else {
            throw HelperInstanceLockError.openFailed(errno)
        }

        guard fcntl(descriptor, F_SETFD, FD_CLOEXEC) == 0 else {
            let error = errno
            close(descriptor)
            throw HelperInstanceLockError.configureFailed(error)
        }

        guard flock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
            let error = errno
            close(descriptor)
            throw HelperInstanceLockError.alreadyRunning(error)
        }

        fileDescriptor = descriptor
    }

    deinit {
        guard fileDescriptor >= 0 else { return }
        flock(fileDescriptor, LOCK_UN)
        close(fileDescriptor)
    }
}

enum HelperInstanceLockError: Error, LocalizedError {
    case openFailed(Int32)
    case configureFailed(Int32)
    case alreadyRunning(Int32)

    var errorDescription: String? {
        switch self {
        case .openFailed(let error):
            return "Failed to open helper instance lock: \(String(cString: strerror(error)))"
        case .configureFailed(let error):
            return "Failed to configure helper instance lock: \(String(cString: strerror(error)))"
        case .alreadyRunning:
            return "Another helper process is already running"
        }
    }
}
