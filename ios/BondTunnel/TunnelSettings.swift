import Foundation
import Network
import NetworkExtension

/// The packet tunnel's addresses, routes, DNS and MTU, from the core's
/// description of the mesh, for a tunnel that takes all of the phone's traffic.
enum TunnelSettings {
    static func make(for mesh: TunnelDescription) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: remoteAddress(of: mesh.aggregator))

        let ipv4 = NEIPv4Settings(addresses: [mesh.address], subnetMasks: [subnetMask(prefix: mesh.prefix)])
        ipv4.includedRoutes = [NEIPv4Route.default()]
        settings.ipv4Settings = ipv4

        // The mesh is IPv4 inside. IPv6 is routed into the tunnel all the same,
        // where it is dropped, so that it cannot go around the bond: apps then
        // fall back to IPv4.
        let ipv6 = NEIPv6Settings(addresses: [uniqueLocalAddress(for: mesh.address)], networkPrefixLengths: [64])
        ipv6.includedRoutes = [NEIPv6Route.default()]
        settings.ipv6Settings = ipv6

        if let servers = mesh.dns, !servers.isEmpty {
            let dns = NEDNSSettings(servers: servers)
            // Without this the tunnel's resolvers are only a fallback and
            // names keep being looked up on Wi-Fi or cellular directly.
            dns.matchDomains = [""]
            settings.dnsSettings = dns
        }

        settings.mtu = NSNumber(value: mesh.mtu)
        return settings
    }

    static func subnetMask(prefix: Int) -> String {
        let bits = min(max(prefix, 0), 32)
        let mask: UInt32 = bits == 0 ? 0 : ~0 << (32 - bits)
        return (0..<4).map { String((mask >> (24 - 8 * $0)) & 0xff) }.joined(separator: ".")
    }

    /// A stable IPv6 address for the interface, which needs one to have IPv6
    /// routes: fd7a:b0ad:: and the last two octets of its IPv4 address.
    static func uniqueLocalAddress(for ipv4: String) -> String {
        let octets = IPv4Address(ipv4)?.rawValue ?? Data([0, 0, 0, 1])
        return String(format: "fd7a:b0ad::%x", Int(octets[2]) << 8 | Int(octets[3]))
    }

    /// NetworkExtension wants an IP address here and only uses it for display;
    /// an aggregator known by name gets a placeholder (as wireguard-apple does).
    static func remoteAddress(of aggregator: String) -> String {
        var host = aggregator
        if host.hasPrefix("["), let end = host.firstIndex(of: "]") {
            host = String(host[host.index(after: host.startIndex)..<end])
        } else if let colon = host.lastIndex(of: ":") {
            host = String(host[..<colon])
        }
        let isAddress = IPv4Address(host) != nil || IPv6Address(host) != nil
        return isAddress ? host : "127.0.0.1"
    }
}
