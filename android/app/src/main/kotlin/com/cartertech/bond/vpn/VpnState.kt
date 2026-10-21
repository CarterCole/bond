package com.cartertech.bond.vpn

import com.cartertech.bond.core.ClientStatus
import kotlinx.coroutines.flow.MutableStateFlow

sealed interface VpnState {
    /** [error] is why the tunnel stopped, when it was not asked to. */
    data class Off(val error: String? = null) : VpnState
    data object Starting : VpnState
    data class Running(val status: ClientStatus) : VpnState
}

/** What BondVpnService is doing, for anything in the process that wants to show it. */
object VpnMonitor {
    val state = MutableStateFlow<VpnState>(VpnState.Off())
}
