import SwiftUI

/// "This phone is the priority device."
struct PrioritySection: View {
    @Environment(AppModel.self) private var model

    private var hub: HubStatus? { model.tunnel.report?.hub }

    var body: some View {
        Section {
            Toggle("Priority Device", isOn: isPriority)
                // The setting lives in the running core, which also stores it.
                .disabled(hub == nil)
            if let hub, hub.isPriorityDevice || hub.realtimePackets > 0 {
                LabeledContent("Priority packets", value: hub.realtimePackets.formatted())
            }
        } footer: {
            Text("Everything this phone sends goes out on its two best connections at once and never waits behind other traffic. Good for calls and live video; it uses more data.")
        }
    }

    private var isPriority: Binding<Bool> {
        Binding {
            hub?.isPriorityDevice ?? false
        } set: { all in
            Task { await model.tunnel.perform(.setPriority(all)) }
        }
    }
}
