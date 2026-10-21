import SwiftUI

/// Lending this phone's connection to a hub: by tethering, which always works,
/// and by the experimental relay inside the tunnel.
struct ShareSection: View {
    @Environment(AppModel.self) private var model

    private var report: TunnelReport? { model.tunnel.report }

    var body: some View {
        Section {
            VStack(alignment: .leading, spacing: 6) {
                Label("Personal Hotspot or USB", systemImage: "personalhotspot").font(.headline)
                Text("1. In Settings › Personal Hotspot, turn on Allow Others to Join.")
                Text("2. Plug this phone into the hub's computer and tap Trust — or join the hub to this phone's hotspot Wi-Fi.")
                Text("The hub sees the phone as one more network connection and bonds it by itself. A cable is best: the hub keeps its own Wi-Fi as well. Bond does not need to be switched on here.")
                    .foregroundStyle(.secondary)
            }
            .font(.subheadline)
            .padding(.vertical, 4)
        } header: {
            Text("Share My Connection")
        }

        Section {
            Toggle(isOn: isSharing) {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Share Over Wi-Fi to Nearby Hubs")
                    Text("Experimental").font(.caption.weight(.semibold)).foregroundStyle(.orange)
                }
            }
            if model.shareEnabled {
                relayStatus
            }
        } footer: {
            Text("Hubs on the same Wi-Fi send part of their traffic, already encrypted, out through this phone's cellular connection. It only runs while Bond is connected — iOS keeps nothing else of the app alive — and iOS may refuse to use cellular for this while the phone is on Wi-Fi, in which case hubs get nothing from it. It uses your cellular data.")
        }
    }

    @ViewBuilder private var relayStatus: some View {
        if let share = report?.share {
            if let problem = share.problem {
                Text(problem).font(.footnote).foregroundStyle(.red)
            } else if let name = share.advertisedAs {
                LabeledContent("Visible to hubs as", value: name)
            } else if share.running {
                Text("Announcing this phone on the network…").foregroundStyle(.secondary)
            }
            if let relay = report?.relay {
                LabeledContent("Hubs using it", value: relay.relay.clients.formatted())
                LabeledContent("Sent for hubs", value: Format.bytes(relay.relay.upBytes))
                LabeledContent("Received for hubs", value: Format.bytes(relay.relay.downBytes))
            }
        } else {
            Text("Starts when Bond is connected.").foregroundStyle(.secondary)
        }
    }

    private var isSharing: Binding<Bool> {
        Binding {
            model.shareEnabled
        } set: { enabled in
            Task { await model.setShare(enabled) }
        }
    }
}
