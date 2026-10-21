import Foundation

/// Numbers as the app shows them, in the same units as the desktop dashboard.
enum Format {
    static func rate(_ bitsPerSecond: Double) -> String {
        switch bitsPerSecond {
        case 1e9...: String(format: "%.2f Gbit/s", bitsPerSecond / 1e9)
        case 1e6...: String(format: "%.1f Mbit/s", bitsPerSecond / 1e6)
        case 1e3...: String(format: "%.0f kbit/s", bitsPerSecond / 1e3)
        default: String(format: "%.0f bit/s", bitsPerSecond)
        }
    }

    static func bytes(_ count: UInt64) -> String {
        Int64(clamping: count).formatted(.byteCount(style: .decimal, spellsOutZero: false))
    }

    static func milliseconds(_ value: Double) -> String {
        String(format: "%.0f ms", value)
    }

    static func percent(_ fraction: Double) -> String {
        String(format: "%.1f %%", fraction * 100)
    }

    static func duration(_ seconds: Int) -> String {
        Duration.seconds(seconds).formatted(
            .units(allowed: [.days, .hours, .minutes, .seconds], width: .narrow, maximumUnitCount: 2))
    }
}
