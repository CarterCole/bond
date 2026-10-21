import Foundation

/// The utun socket behind the packet tunnel.
///
/// NEPacketTunnelProvider offers packets only through `packetFlow`, but the
/// core reads and writes the device itself. The descriptor is in this process
/// all the same, so it is found by looking (the technique wireguard-apple
/// uses): the one open socket that is a kernel control socket and answers to a
/// utun interface name.
enum TunnelFileDescriptor {
    // From <sys/sys_domain.h> and <net/if_utun.h>, which the iOS SDK leaves out.
    private static let AF_SYS_CONTROL: UInt16 = 2
    private static let SYSPROTO_CONTROL: Int32 = 2
    private static let UTUN_OPT_IFNAME: Int32 = 2

    static func find() -> Int32? {
        (0...1024).first { isKernelControlSocket($0) && interfaceName($0)?.hasPrefix("utun") == true }
    }

    /// A copy of the descriptor for the core, which owns and closes what it is
    /// given; the original stays with NetworkExtension.
    static func duplicateForCore(_ fd: Int32) throws -> Int32 {
        let copy = dup(fd)
        guard copy >= 0 else { throw POSIXError(.init(rawValue: errno) ?? .EBADF) }
        return copy
    }

    private static func isKernelControlSocket(_ fd: Int32) -> Bool {
        // The peer is a struct sockaddr_ctl:
        //   u_char sc_len; u_char sc_family; u_int16_t ss_sysaddr; …
        var address = sockaddr_storage()
        var length = socklen_t(MemoryLayout<sockaddr_storage>.size)
        let found = withUnsafeMutablePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getpeername(fd, $0, &length) == 0 }
        }
        guard found, Int32(address.ss_family) == AF_SYSTEM else { return false }
        return withUnsafeBytes(of: address) { $0.load(fromByteOffset: 2, as: UInt16.self) } == AF_SYS_CONTROL
    }

    private static func interfaceName(_ fd: Int32) -> String? {
        var name = [CChar](repeating: 0, count: Int(IFNAMSIZ))
        var length = socklen_t(name.count)
        guard getsockopt(fd, SYSPROTO_CONTROL, UTUN_OPT_IFNAME, &name, &length) == 0 else { return nil }
        return name.withUnsafeBufferPointer { String(cString: $0.baseAddress!) }
    }
}
