package com.cartertech.bond.vpn

import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.Network
import android.net.VpnService
import android.util.Log
import androidx.core.app.ServiceCompat
import com.cartertech.bond.Notifications
import com.cartertech.bond.R
import com.cartertech.bond.bond
import com.cartertech.bond.core.BondPlatform
import com.cartertech.bond.core.ClientStatus
import com.cartertech.bond.core.CoreDispatcher
import com.cartertech.bond.core.MeshInfo
import com.cartertech.bond.core.Uplinks
import com.cartertech.bond.core.awaitNextPoll
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import mobile.Client
import mobile.Mobile
import org.json.JSONArray
import org.json.JSONObject
import java.net.InetAddress
import java.net.NetworkInterface
import java.util.concurrent.ConcurrentHashMap

/**
 * "Bond this phone": a VPN whose far end is the aggregator, carried over Wi-Fi and mobile data at once.
 *
 * Android supplies the TUN device and the networks; the Go core does everything in between.
 */
class BondVpnService : VpnService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // Read from binder threads too: that is where onRevoke arrives.
    @Volatile
    private var session: Job? = null

    /** Only touched on [CoreDispatcher], so that nothing is ever called on a client that was stopped. */
    private var client: Client? = null

    @Volatile
    private var notified: String? = null

    /** How many times each uplink's network has been replaced: the "gen" the core is given. */
    private val generations = ConcurrentHashMap<String, Int>()

    override fun onCreate() {
        super.onCreate()
        running = this
    }

    // The type constant is newer than minSdk; ServiceCompat leaves it out where Android does not know it.
    @SuppressLint("InlinedApi")
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_DISCONNECT) {
            disconnect()
            return START_NOT_STICKY
        }
        // Anything else is a request to connect: the app's switch, Android's always-on VPN, or Android
        // bringing the service back (null intent).
        try {
            val text = getString(R.string.vpn_connecting)
            ServiceCompat.startForeground(
                this,
                Notifications.VPN_ID,
                notification(text),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED,
            )
            notified = text
        } catch (e: Exception) {
            // Android refuses the systemExempted type to an app the user has not approved as a VPN.
            Log.e(TAG, "cannot run in the foreground", e)
            VpnMonitor.state.value = VpnState.Off(e.message)
            stopSelf()
            return START_NOT_STICKY
        }
        val previous = session
        if (previous?.isActive != true) session = launchSession(previous)
        return START_STICKY
    }

    /** The user took the VPN away in Settings, or another VPN app took over. */
    override fun onRevoke() = disconnect()

    override fun onDestroy() {
        scope.cancel() // the session cleans up after itself on its way out
        if (running === this) running = null
        super.onDestroy()
    }

    private fun disconnect() {
        val active = session
        if (active?.isActive == true) active.cancel() else stopSelf()
    }

    private fun launchSession(previous: Job?): Job = scope.launch {
        val me = coroutineContext[Job]
        try {
            previous?.join() // switched off and straight back on: let the old tunnel finish closing
            runSession()
        } finally {
            withContext(NonCancellable + Dispatchers.Main) {
                // Unless it has already been switched back on.
                if (session === me) {
                    ServiceCompat.stopForeground(this@BondVpnService, ServiceCompat.STOP_FOREGROUND_REMOVE)
                    stopSelf()
                }
            }
        }
    }

    private suspend fun runSession() {
        var error: String? = null
        VpnMonitor.state.value = VpnState.Starting
        Uplinks.acquireAll(this)
        try {
            val config = withContext(Dispatchers.IO) { bond.config.refreshed() }
                ?: throw IllegalStateException(getString(R.string.error_not_joined))
            // Android answers the network requests within moments. Waiting for the first answer lets the
            // tunnel start with its paths instead of picking them up on the core's next round.
            val networks = withTimeoutOrNull(FIRST_NETWORK_MS) { Uplinks.available.first { it.isNotEmpty() } }
                ?: emptyMap()
            val address = withContext(CoreDispatcher) { open(config, networks) }
            coroutineScope {
                val follower = launch { followNetworks(networks) }
                while (onClient { publish(it) } != null) {
                    // The core closes the TUN device when it can no longer read from it, and Android then
                    // takes the VPN down without telling the service. Do not sit here looking connected
                    // while traffic goes around the tunnel.
                    if (!tunnelExists(address)) throw IllegalStateException(getString(R.string.error_tunnel_closed))
                    awaitNextPoll(VpnMonitor.state.subscriptionCount)
                }
                follower.cancel()
            }
        } catch (e: CancellationException) {
            // Disconnected, revoked or destroyed.
        } catch (e: Exception) {
            Log.e(TAG, "tunnel failed", e)
            error = e.message ?: e.toString()
        } finally {
            withContext(NonCancellable) {
                withContext(CoreDispatcher) {
                    client?.stop()
                    client = null
                    VpnMonitor.state.value = VpnState.Off(error)
                }
                Uplinks.releaseAll(this@BondVpnService)
            }
        }
    }

    /** Creates the TUN device and hands it to the core. Returns the tunnel's address. */
    private fun open(config: String, networks: Map<String, Network>): String {
        val mesh = MeshInfo.parse(Mobile.describe(config))
        val tun = Builder()
            .setSession(getString(R.string.app_name))
            .setMtu(mesh.mtu)
            .addAddress(mesh.address, mesh.prefix)
            // Everything IPv4 goes through the bond. No IPv6 address or route is added, which makes
            // Android block IPv6 while the VPN is up rather than let it leak around the tunnel.
            .addRoute("0.0.0.0", 0)
            .apply { mesh.dns.forEach { addDnsServer(it) } }
            // protect() only reaches sockets opened while this service exists. A relay that was already
            // sharing has sockets from before, and a VPN's routing outranks the network a socket is
            // bound to: without this, switching bonding on would pull the relay's traffic into the
            // tunnel. Nothing in this app is meant to go through the bond anyway.
            .addDisallowedApplication(packageName)
            // Metered when an underlying network is, rather than always.
            .setMetered(false)
            .setUnderlyingNetworks(networks.values.toTypedArray())
            .setConfigureIntent(Notifications.openApp(this))
            .establish()
            ?: throw IllegalStateException(getString(R.string.error_vpn_permission))
        // From here the descriptor is the core's: it closes it, which is also what takes the VPN down.
        client = Mobile.startClient(config, tun.detachFd(), networksJson(networks), BondPlatform)
        return mesh.address
    }

    /** Whether the TUN interface still exists: it is the one that carries the tunnel's address. */
    private fun tunnelExists(address: String): Boolean =
        runCatching { NetworkInterface.getByInetAddress(InetAddress.getByName(address)) != null }
            .getOrDefault(true)

    /**
     * Keeps the core's list of networks matching Android's.
     *
     * The core opens one socket per network name and binds it once. When Android replaces a network
     * (Wi-Fi roams to another access point, mobile data reconnects) the name stays and the socket is
     * left on a network that is gone; a new "gen" for that name tells the core to open a fresh one.
     */
    private suspend fun followNetworks(initial: Map<String, Network>) {
        var listed = initial
        Uplinks.available.collect { now ->
            for ((name, network) in now) {
                if (listed[name] != network) generations.merge(name, 1, Int::plus)
            }
            if (now != listed) onClient { it.setNetworks(networksJson(now)) }
            setUnderlyingNetworks(now.values.toTypedArray())
            listed = now
        }
    }

    private fun networksJson(networks: Map<String, Network>): String {
        val list = JSONArray()
        for (uplink in Uplinks.all) {
            if (uplink.name !in networks) continue
            list.put(
                JSONObject()
                    .put("name", uplink.name)
                    .put("policy", uplink.defaultPolicy.wire)
                    .put("gen", generations[uplink.name] ?: 0),
            )
        }
        return list.toString()
    }

    private suspend fun <T> onClient(block: (Client) -> T): T? =
        withContext(CoreDispatcher) { client?.let(block) }

    /** Applies a setting to the running core and stores the config it produces. */
    private suspend fun change(block: (Client) -> Unit) {
        onClient {
            block(it)
            bond.config.update(it.configJSON())
            publish(it)
        }
    }

    /** Runs on [CoreDispatcher], so that a status can never be shown after the tunnel has closed. */
    private fun publish(client: Client) {
        val status = ClientStatus.parse(client.status())
        VpnMonitor.state.value = VpnState.Running(status)
        val text = summary(status)
        if (text != notified) {
            notified = text
            Notifications.update(this, Notifications.VPN_ID, notification(text))
        }
    }

    private fun summary(status: ClientStatus): String {
        if (!status.connected) return getString(R.string.vpn_connecting)
        val up = status.paths.count { it.up }
        return resources.getQuantityString(R.plurals.vpn_connected_paths, up, up)
    }

    private fun notification(text: String) = Notifications.status(
        this,
        title = getString(R.string.mode_bond),
        text = text,
        stopLabel = getString(R.string.action_disconnect),
        stopAction = ACTION_DISCONNECT,
    )

    companion object {
        private const val TAG = "BondVpn"
        private const val ACTION_DISCONNECT = "com.cartertech.bond.vpn.DISCONNECT"
        private const val FIRST_NETWORK_MS = 1_500L

        /** The service, while it exists: sockets are protected through it. */
        @Volatile
        var running: BondVpnService? = null
            private set

        /** Call after VpnService.prepare() has been agreed to, with the app in the foreground. */
        fun start(context: Context) {
            context.startService(Intent(context, BondVpnService::class.java))
        }

        fun stop() {
            running?.disconnect()
        }

        suspend fun setPolicy(path: String, policy: String) {
            running?.change { it.setPolicy(path, policy) }
        }

        suspend fun setPriority(all: Boolean) {
            running?.change { it.setPriority(all) }
        }
    }
}
