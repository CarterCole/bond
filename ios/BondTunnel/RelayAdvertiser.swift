import dnssd
import Foundation
import os

/// Announces the relay on the local network by Bonjour, so that hubs find it.
///
/// The Go relay already owns the UDP port. This only registers the service
/// with the system's mDNS responder and binds nothing — which is why it is
/// DNSServiceRegister and not NWListener, which would want the port itself.
final class RelayAdvertiser: @unchecked Sendable {
    enum State: Equatable, Sendable {
        case pending
        case advertised(as: String)
        case failed(String)
    }

    private let queue = DispatchQueue(label: "com.cartertech.bond.tunnel.bonjour")
    private var service: DNSServiceRef?  // only touched on `queue`
    private let current = OSAllocatedUnfairLock(initialState: State.pending)

    var state: State { current.withLock { $0 } }

    deinit { stop() }

    /// - Parameters:
    ///   - type: the DNS-SD service type, "_bond-relay._udp".
    ///   - txt: the TXT entries, each "key=value".
    func start(name: String, type: String, port: Int32, txt: [String]) {
        queue.sync {
            deallocate()
            let record = Self.txtRecord(txt)
            var service: DNSServiceRef?
            let error = DNSServiceRegister(
                &service, 0, 0, Self.instanceName(name), type, nil, nil,
                UInt16(clamping: port).bigEndian, UInt16(record.count), record,
                { _, flags, error, name, _, _, context in
                    guard let context else { return }
                    let advertiser = Unmanaged<RelayAdvertiser>.fromOpaque(context).takeUnretainedValue()
                    advertiser.registered(flags: flags, error: error, name: name.map { String(cString: $0) })
                },
                // Unretained: `stop` (and so deinit) ends the registration on
                // `queue` before this object can go away.
                Unmanaged.passUnretained(self).toOpaque())
            guard error == kDNSServiceErr_NoError, let service else {
                set(.failed(Self.describe(error)))
                return
            }
            DNSServiceSetDispatchQueue(service, queue)
            self.service = service
        }
    }

    func stop() {
        queue.sync { deallocate() }
    }

    private func deallocate() {
        if let service { DNSServiceRefDeallocate(service) }
        service = nil
        set(.pending)
    }

    private func registered(flags: DNSServiceFlags, error: DNSServiceErrorType, name: String?) {
        if error != kDNSServiceErr_NoError {
            set(.failed(Self.describe(error)))
        } else if flags & DNSServiceFlags(kDNSServiceFlagsAdd) != 0, let name {
            set(.advertised(as: name))
        } else {
            set(.pending)  // the name was taken; the responder is choosing another
        }
    }

    private func set(_ state: State) {
        current.withLock { $0 = state }
    }

    /// A DNS TXT record: each entry preceded by its length in one byte.
    static func txtRecord(_ entries: [String]) -> [UInt8] {
        entries.reduce(into: []) { record, entry in
            let bytes = Array(entry.utf8)
            guard (1...255).contains(bytes.count) else { return }
            record.append(UInt8(bytes.count))
            record.append(contentsOf: bytes)
        }
    }

    /// The device name as a DNS-SD instance label, by the same rule as the
    /// core's own announcement on desktops (discovery.sanitize). Hubs read the
    /// real name from the TXT record.
    static func instanceName(_ name: String) -> String {
        var label = name
            .replacingOccurrences(of: ".", with: "-")
            .replacingOccurrences(of: "\\", with: "-")
            .trimmingCharacters(in: .whitespaces)
        while label.utf8.count > 60 { label.removeLast() }
        return label.isEmpty ? "bond-relay" : label
    }

    private static func describe(_ error: DNSServiceErrorType) -> String {
        if error == kDNSServiceErr_PolicyDenied {
            return "Local network access is off for Bond. Turn it on in Settings › Privacy & Security › Local Network."
        }
        return "Could not announce this phone on the network (Bonjour error \(error))."
    }
}
