//go:build !linux

package bond

// Elsewhere WireGuard allocates batch × max-datagram buffers per receive
// loop; that matters inside an iOS network extension's memory ceiling.
const batchSize = 1
