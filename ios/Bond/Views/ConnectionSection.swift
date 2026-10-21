import NetworkExtension
import SwiftUI

/// The "Bond this phone" switch and what the tunnel is doing.
struct ConnectionSection: View {
    @Environment(AppModel.self) private var model
    let device: TunnelDescription

    private var tunnel: TunnelController { model.tunnel }
    private var hub: HubStatus? { tunnel.report?.hub }

    var body: some View {
        Section {
            Toggle(isOn: isOn) {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Bond This Phone")
                    Text(statusText)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            }
            if let problem = tunnel.problem {
                Text(problem)
                    .font(.footnote)
                    .foregroundStyle(.red)
            }
            if let hub {
                LabeledContent("Receiving", value: Format.rate(hub.paths.reduce(0) { $0 + $1.rxBps }))
                LabeledContent("Sending", value: Format.rate(hub.paths.reduce(0) { $0 + $1.txBps }))
                LabeledContent("Connected for", value: Format.duration(hub.uptimeSec))
            }
            LabeledContent("Name", value: device.name)
            LabeledContent("Address", value: device.address)
            LabeledContent("Server", value: device.aggregator)
        } footer: {
            Text("Uses Wi-Fi and cellular at the same time for everything this phone does.")
        }
    }

    private var isOn: Binding<Bool> {
        Binding {
            tunnel.isOn
        } set: { on in
            if on {
                Task { await tunnel.connect(to: device) }
            } else {
                tunnel.disconnect()
            }
        }
    }

    private var statusText: String {
        switch tunnel.status {
        case .connecting: "Starting…"
        case .connected:
            switch hub {
            case nil: "Connected"
            case let hub?:
                hub.connected
                    ? "Connected over \(hub.paths.filter(\.up).count) of \(hub.paths.count) connections"
                    : "Reaching the server…"
            }
        case .reasserting: "Reconnecting…"
        case .disconnecting: "Stopping…"
        default: tunnel.isOn ? "Starting…" : "Off"
        }
    }
}
