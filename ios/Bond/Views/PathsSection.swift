import SwiftUI

/// One card per path the bond is using or could use.
struct PathsSection: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Section {
            if let hub = model.tunnel.report?.hub {
                if hub.paths.isEmpty {
                    Text("Looking for connections…").foregroundStyle(.secondary)
                }
                ForEach(hub.paths) { path in
                    PathCard(path: path) { policy in
                        Task { await model.tunnel.perform(.setPolicy(path: path.name, policy: policy.rawValue)) }
                    }
                }
            } else {
                Text("Switch Bond on to see this phone's connections.").foregroundStyle(.secondary)
            }
        } header: {
            Text("Connections")
        } footer: {
            Text("Primary connections are used first. Bonded ones add their speed when it helps. Backup ones stay idle until nothing else works.")
        }
    }
}

struct PathCard: View {
    let path: PathStatus
    let setPolicy: (PathPolicy) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label(title, systemImage: symbol).font(.headline)
                Spacer()
                Text(path.up ? "Up" : "Down")
                    .font(.caption.weight(.semibold))
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background((path.up ? Color.green : Color.red).opacity(0.18), in: Capsule())
                    .foregroundStyle(path.up ? .green : .red)
            }
            Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 6) {
                GridRow {
                    Metric(label: "Receiving", value: Format.rate(path.rxBps))
                    Metric(label: "Sending", value: Format.rate(path.txBps))
                }
                GridRow {
                    Metric(label: "Round trip", value: path.up ? Format.milliseconds(path.rttMs) : "–")
                    Metric(label: "Loss", value: Format.percent(path.loss))
                }
                GridRow {
                    Metric(label: "Received", value: Format.bytes(path.rxBytes))
                    Metric(label: "Sent", value: Format.bytes(path.txBytes))
                }
            }
            Picker("Use as", selection: policy) {
                ForEach(PathPolicy.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
        }
        .padding(.vertical, 4)
    }

    private var policy: Binding<PathPolicy> {
        Binding {
            PathPolicy(rawValue: path.policy) ?? .bonded
        } set: {
            setPolicy($0)
        }
    }

    private var title: String {
        switch path.kind {
        case "relay": "Relay " + path.name.dropFirst("relay:".count)
        case "ws": Self.uplinkName(String(path.name.dropFirst("ws:".count))) + " (fallback)"
        default: Self.uplinkName(path.name)
        }
    }

    private var symbol: String {
        switch (path.kind, path.name.split(separator: ":").last.map(String.init) ?? path.name) {
        case ("relay", _): "arrow.triangle.branch"
        case (_, "wifi"): "wifi"
        case (_, "cellular"): "antenna.radiowaves.left.and.right"
        default: "cable.connector"
        }
    }

    private static func uplinkName(_ name: String) -> String {
        switch name {
        case "wifi": "Wi-Fi"
        case "cellular": "Cellular"
        case "ethernet": "Ethernet"
        default: name
        }
    }
}

private struct Metric: View {
    let label: String
    let value: String

    var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(value).font(.callout.monospacedDigit())
            Text(label).font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
