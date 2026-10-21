package com.cartertech.bond.relay

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.util.Log
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import mobile.Mobile

/** Announces the relay on the local network (DNS-SD) so that hubs find it without being told. */
class RelayAdvertiser(context: Context) {
    private val nsd = context.applicationContext.getSystemService(NsdManager::class.java)
    private val _state = MutableStateFlow(Announcement.Pending)
    val state: StateFlow<Announcement> = _state

    private var registration: Registration? = null

    /** Does nothing if this port is already announced. */
    @Synchronized
    fun announce(name: String, port: Int, txt: List<String>) {
        if (registration?.port == port) return
        withdraw()
        val info = NsdServiceInfo().apply {
            serviceName = instanceName(name)
            serviceType = Mobile.relayServiceType()
            this.port = port
            for (entry in txt) setAttribute(entry.substringBefore('='), entry.substringAfter('=', ""))
        }
        val listener = Registration(port)
        try {
            nsd.registerService(info, NsdManager.PROTOCOL_DNS_SD, listener)
            registration = listener
        } catch (e: RuntimeException) {
            // Android 17 without the local network permission, or a name or TXT entry it will not take.
            Log.w(TAG, "cannot announce the relay", e)
            _state.value = Announcement.Failed
        }
    }

    @Synchronized
    fun withdraw() {
        registration?.let { runCatching { nsd.unregisterService(it) } }
        registration = null
        _state.value = Announcement.Pending
    }

    @Synchronized
    private fun report(from: Registration, state: Announcement) {
        if (from === registration) _state.value = state
    }

    private inner class Registration(val port: Int) : NsdManager.RegistrationListener {
        override fun onServiceRegistered(info: NsdServiceInfo) = report(this, Announcement.Announced)

        override fun onRegistrationFailed(info: NsdServiceInfo, errorCode: Int) {
            Log.w(TAG, "announcing the relay failed: NsdManager error $errorCode")
            report(this, Announcement.Failed)
        }

        override fun onServiceUnregistered(info: NsdServiceInfo) = Unit

        override fun onUnregistrationFailed(info: NsdServiceInfo, errorCode: Int) = Unit
    }

    private companion object {
        const val TAG = "BondRelay"

        // The same tidying the core does for desktop relays: a DNS-SD instance name is one label of at
        // most 63 bytes. Hubs read the real name from the TXT record.
        fun instanceName(name: String): String {
            var label = name.trim().replace('.', '-').replace('\\', '-').ifEmpty { "bond-relay" }
            while (label.encodeToByteArray().size > 60) label = label.dropLast(1)
            return label
        }
    }
}
