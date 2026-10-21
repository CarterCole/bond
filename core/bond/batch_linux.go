package bond

import "golang.zx2c4.com/wireguard/conn"

// Linux and Android TUN devices batch, so let WireGuard hand us full batches.
const batchSize = conn.IdealBatchSize
