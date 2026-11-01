import Darwin
import Foundation

/// Reads the app's memory footprint: the same number Xcode's memory gauge and the
/// iOS jetsam limit use (phys_footprint from TASK_VM_INFO).
enum Memory {
    static func footprintBytes() -> UInt64? {
        var info = task_vm_info_data_t()
        var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
        let result = withUnsafeMutablePointer(to: &info) { pointer in
            pointer.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
                task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count)
            }
        }
        guard result == KERN_SUCCESS else { return nil }
        return info.phys_footprint
    }

    /// The most the system lets this process allocate right now (iOS 13+), or nil.
    static func availableBytes() -> UInt64? {
        let value = os_proc_available_memory()
        return value > 0 ? UInt64(value) : nil
    }

    static func megabytes(_ bytes: UInt64) -> String {
        String(format: "%.0f MB", Double(bytes) / 1_048_576)
    }
}
