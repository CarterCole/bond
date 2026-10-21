import Foundation

/// Runs work that blocks its thread (the core's network calls) on a GCD
/// thread, leaving the main thread and Swift's small concurrency pool free.
func blocking<T: Sendable>(_ work: @escaping @Sendable () throws -> T) async throws -> T {
    try await withCheckedThrowingContinuation { continuation in
        DispatchQueue.global(qos: .userInitiated).async {
            continuation.resume(with: Result { try work() })
        }
    }
}
