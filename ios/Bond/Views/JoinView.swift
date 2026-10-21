import AVFoundation
import SwiftUI
import VisionKit

/// The first screen: join a mesh with the code its server shows.
struct JoinView: View {
    @Environment(AppModel.self) private var model

    @State private var code = ""
    @State private var deviceName = UIDevice.current.name
    @State private var isScanning = false
    @State private var isJoining = false
    @State private var scannerNote: String?
    @State private var failure: String?

    private var trimmedCode: String { code.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var trimmedName: String { deviceName.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Button {
                        Task { await openScanner() }
                    } label: {
                        Label("Scan QR Code", systemImage: "qrcode.viewfinder")
                    }
                    TextField("bond://server:7777/…", text: $code, axis: .vertical)
                        .font(.callout.monospaced())
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                        .lineLimit(2...4)
                    PasteButton(payloadType: String.self) { strings in
                        Task { @MainActor in code = strings.first ?? code }
                    }
                } header: {
                    Text("Join code")
                } footer: {
                    Text(scannerNote ?? "Your Bond server shows the code when it starts. Scan it, or paste it here.")
                }

                Section {
                    TextField("Name", text: $deviceName)
                        .autocorrectionDisabled()
                } header: {
                    Text("This phone")
                } footer: {
                    Text("How this phone appears to the rest of the mesh.")
                }

                Section {
                    Button {
                        Task { await join() }
                    } label: {
                        HStack {
                            Text(isJoining ? "Joining…" : "Join")
                            Spacer()
                            if isJoining { ProgressView() }
                        }
                    }
                    .disabled(isJoining || trimmedCode.isEmpty || trimmedName.isEmpty)
                } footer: {
                    if let message = failure ?? model.storageProblem {
                        Text(message).foregroundStyle(.red)
                    }
                }
            }
            .navigationTitle("Join a Mesh")
            .sheet(isPresented: $isScanning) {
                ScannerSheet { scanned in
                    code = scanned
                    isScanning = false
                    Task { await join() }
                }
            }
        }
    }

    private func openScanner() async {
        guard DataScannerViewController.isSupported else {
            scannerNote = "This device cannot scan codes. Paste the join code instead."
            return
        }
        guard await AVCaptureDevice.requestAccess(for: .video) else {
            scannerNote = "Bond is not allowed to use the camera. Allow it in Settings, or paste the join code."
            return
        }
        scannerNote = nil
        isScanning = true
    }

    private func join() async {
        guard !isJoining else { return }
        isJoining = true
        failure = nil
        defer { isJoining = false }
        do {
            try await model.join(code: trimmedCode, deviceName: trimmedName)
        } catch {
            failure = error.localizedDescription
        }
    }
}

private struct ScannerSheet: View {
    @Environment(\.dismiss) private var dismiss
    let onScan: (String) -> Void

    var body: some View {
        NavigationStack {
            QRScannerView(onScan: onScan)
                .ignoresSafeArea(edges: .bottom)
                .navigationTitle("Scan Join Code")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") { dismiss() }
                    }
                }
        }
    }
}
