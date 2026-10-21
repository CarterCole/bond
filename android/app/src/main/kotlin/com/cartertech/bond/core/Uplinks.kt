package com.cartertech.bond.core

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.util.Log
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.conflate
import kotlinx.coroutines.flow.distinctUntilChanged

/**
 * One kind of connection the phone has. While anything holds it, Android is asked to keep a network of
 * that kind up, and [network] is the one to bind sockets to.
 *
 * Held by reference count, because the VPN and the relay both need the same networks and can run at
 * the same time.
 */
class Uplink(val name: String, private val transport: Int, val defaultPolicy: Policy) {
    private val _network = MutableStateFlow<Network?>(null)
    val network: StateFlow<Network?> = _network

    private var holders = 0
    private var callback: ConnectivityManager.NetworkCallback? = null

    @Synchronized
    fun acquire(context: Context) {
        if (holders++ > 0) return
        val request = NetworkRequest.Builder()
            .addTransportType(transport)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) = set(this, null, network)

            // Only reported when no other network of this kind takes its place; a replacement
            // simply arrives through onAvailable.
            override fun onLost(network: Network) = set(this, network, null)
        }
        val cm = context.connectivity
        try {
            // requestNetwork, not registerNetworkCallback: a request is what makes Android bring mobile
            // data up, and keep it up, while Wi-Fi is connected.
            cm.requestNetwork(request, cb)
            callback = cb
        } catch (e: RuntimeException) {
            // Some Android 11 builds refuse requestNetwork (SecurityException) to apps that do hold
            // CHANGE_NETWORK_STATE. Watching still finds the network whenever something else has it up.
            Log.w(TAG, "cannot request $name, only watching it", e)
            runCatching { cm.registerNetworkCallback(request, cb) }.onSuccess { callback = cb }
        }
    }

    @Synchronized
    fun release(context: Context) {
        if (holders == 0 || --holders > 0) return
        callback?.let { runCatching { context.connectivity.unregisterNetworkCallback(it) } }
        callback = null
        _network.value = null
    }

    @Synchronized
    private fun set(from: ConnectivityManager.NetworkCallback, expect: Network?, value: Network?) {
        if (from !== callback) return // a callback that was already unregistered
        if (expect == null || _network.value == expect) _network.value = value
    }

    private val Context.connectivity get() = getSystemService(ConnectivityManager::class.java)

    private companion object {
        const val TAG = "BondUplink"
    }
}

object Uplinks {
    val wifi = Uplink("wifi", NetworkCapabilities.TRANSPORT_WIFI, Policy.Primary)
    val cellular = Uplink("cellular", NetworkCapabilities.TRANSPORT_CELLULAR, Policy.Bonded)
    val all = listOf(wifi, cellular)

    fun named(name: String): Uplink? = all.firstOrNull { it.name == name }

    /** The networks that are up right now, by uplink name. Slow collectors see only the latest. */
    val available: Flow<Map<String, Network>> =
        combine(all.map { it.network }) { networks ->
            buildMap { all.forEachIndexed { i, uplink -> networks[i]?.let { put(uplink.name, it) } } }
        }.distinctUntilChanged().conflate()

    fun acquireAll(context: Context) = all.forEach { it.acquire(context.applicationContext) }

    fun releaseAll(context: Context) = all.forEach { it.release(context.applicationContext) }
}
