import SwiftUI

/// The screen for a phone that has joined a mesh.
struct MainView: View {
    @Environment(AppModel.self) private var model
    let device: TunnelDescription

    @State private var isConfirmingLeave = false
    @State private var leaveFailure: String?

    var body: some View {
        NavigationStack {
            List {
                ConnectionSection(device: device)
                PathsSection()
                PrioritySection()
                ShareSection()
                Section {
                    Button("Leave Mesh", role: .destructive) { isConfirmingLeave = true }
                        .confirmationDialog(
                            "Leave this mesh?", isPresented: $isConfirmingLeave, titleVisibility: .visible
                        ) {
                            Button("Leave Mesh", role: .destructive) {
                                Task { await leave() }
                            }
                        } message: {
                            Text("This removes Bond's VPN configuration and this phone's keys. To come back you will need a new join code.")
                        }
                } footer: {
                    VStack(alignment: .leading, spacing: 4) {
                        if let leaveFailure {
                            Text(leaveFailure).foregroundStyle(.red)
                        }
                        Text("Bond core \(BondCore.version)")
                    }
                }
            }
            .navigationTitle("Bond")
            .task(id: model.tunnel.status) { await model.tunnel.monitor() }
        }
    }

    private func leave() async {
        do {
            try await model.leave()
        } catch {
            leaveFailure = error.localizedDescription
        }
    }
}
