package com.cartertech.bond.relay

import com.cartertech.bond.core.RelayStatus
import kotlinx.coroutines.flow.MutableStateFlow

/** Whether hubs can find this phone by themselves (DNS-SD on the local network). */
enum class Announcement { Pending, Announced, Failed }

sealed interface RelayState {
    /** [error] is why sharing stopped, when it was not asked to. */
    data class Off(val error: String? = null) : RelayState
    data object Starting : RelayState

    /** Sharing is on but there is no mobile data to share; [error] is set when the relay could not start. */
    data class Waiting(val error: String? = null) : RelayState

    data class Running(
        val status: RelayStatus,
        val announcement: Announcement,
        /** "ip:port" on the Wi-Fi network, for adding this phone to a hub by hand. */
        val lanAddress: String?,
    ) : RelayState
}

/** What RelayService is doing, for anything in the process that wants to show it. */
object RelayMonitor {
    val state = MutableStateFlow<RelayState>(RelayState.Off())
}
