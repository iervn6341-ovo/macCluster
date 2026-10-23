import Foundation
import Darwin

/// A deadline must release the caller even when a command ignores SIGTERM. The lock protects the
/// expiry result read by the subprocess task from the timer queue; the timer is cancelled on success.
final class ProcessDeadline: @unchecked Sendable {
    private let lock = NSLock()
    private var didExpire = false
    var expired: Bool {
        lock.lock(); defer { lock.unlock() }
        return didExpire
    }
    func expire(_ process: Process) {
        lock.lock()
        guard process.isRunning else { lock.unlock(); return }
        didExpire = true
        process.terminate()
        lock.unlock()
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) {
            if process.isRunning { kill(process.processIdentifier, SIGKILL) }
        }
    }
}
