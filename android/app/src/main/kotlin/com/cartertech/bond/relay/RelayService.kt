package com.cartertech.bond.relay

import android.annotation.SuppressLint
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.wifi.WifiManager
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.text.format.Formatter
import android.util.Log
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import com.cartertech.bond.Notifications
import com.cartertech.bond.R
import com.cartertech.bond.bond
import com.cartertech.bond.core.BondPlatform
import com.cartertech.bond.core.CoreDispatcher
import com.cartertech.bond.core.LocalNetwork
import com.cartertech.bond.core.MeshInfo
import com.cartertech.bond.core.RelayStatus
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
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import mobile.Mobile
import mobile.Relay
import java.net.Inet4Address

/**
 * "Share my connection": hubs on the same Wi-Fi send their (already encrypted) datagrams here, and the
 * core forwards them to the aggregator over this phone's mobile data.
 *
 * The service's job is everything around the core: keeping mobile data up next to Wi-Fi, keeping the
 * phone awake, announcing the relay, and telling the core when a network has been replaced.
 */
class RelayService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private var session: Job? = null
    private lateinit var advertiser: RelayAdvertiser
    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null

    // Read and written on [CoreDispatcher] only, so nothing is ever called on a relay that was stopped.
    private var relay: Relay? = null

    @Volatile
    private var notified: String? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        running = this
        advertiser = RelayAdvertiser(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            bond.prefs.shareEnabled = false
            shutDown()
            return START_NOT_STICKY
        }
        try {
            val text = getString(R.string.share_starting)
            ServiceCompat.startForeground(
                this,
                Notifications.RELAY_ID,
                notification(text),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE,
            )
            notified = text
        } catch (e: Exception) {
            Log.e(TAG, "cannot run in the foreground", e)
            RelayMonitor.state.value = RelayState.Off(e.message)
            stopSelf()
            return START_NOT_STICKY
        }
        bond.prefs.shareEnabled = true
        val previous = session
        if (previous?.isActive != true) session = launchSession(previous)
        return START_STICKY
    }

    override fun onDestroy() {
        scope.cancel() // the session cleans up after itself on its way out
        if (running === this) running = null
        super.onDestroy()
    }

    private fun shutDown() {
        val active = session
        if (active?.isActive == true) active.cancel() else stopSelf()
    }

    private fun launchSession(previous: Job?): Job = scope.launch {
        val me = coroutineContext[Job]
        try {
            previous?.join() // switched off and straight back on: let the old relay finish closing
            runSession()
        } finally {
            withContext(NonCancellable + Dispatchers.Main) {
                // Unless it has already been switched back on.
                if (session === me) {
                    ServiceCompat.stopForeground(this@RelayService, ServiceCompat.STOP_FOREGROUND_REMOVE)
                    stopSelf()
                }
            }
        }
    }

    private suspend fun runSession() {
        var error: String? = null
        RelayMonitor.state.value = RelayState.Starting
        Uplinks.acquireAll(this)
        acquireLocks()
        try {
            // Started from the tile or at boot, nothing has checked this yet; and without it the relay
            // would look alive while Android drops everything hubs send.
            if (LocalNetwork.permissionMissing(this)) {
                throw IllegalStateException(getString(R.string.local_network_denied))
            }
            // Looks the aggregator up while that is certain to work; the core falls back on the answer.
            val config = withContext(Dispatchers.IO) { bond.config.refreshed() }
                ?: throw IllegalStateException(getString(R.string.error_not_joined))
            val mesh = withContext(CoreDispatcher) { MeshInfo.parse(Mobile.describe(config)) }
            // A failure here is one that passes: the port still held by a relay that is closing, say.
            while (!start(config, mesh)) delay(RETRY_MS)
            coroutineScope {
                launch { followNetworks() }
                while (true) {
                    withContext(CoreDispatcher) { relay?.let { publish(it) } }
                    awaitNextPoll(RelayMonitor.state.subscriptionCount)
                }
            }
        } catch (e: CancellationException) {
            // Switched off or destroyed.
        } catch (e: Exception) {
            Log.e(TAG, "sharing failed", e)
            error = e.message ?: e.toString()
            bond.prefs.shareEnabled = false
        } finally {
            withContext(NonCancellable) {
                withContext(CoreDispatcher) {
                    close()
                    advertiser.withdraw()
                    RelayMonitor.state.value = RelayState.Off(error)
                }
                releaseLocks()
                Uplinks.releaseAll(this@RelayService)
            }
        }
    }

    /** Returns false if the relay could not be started. */
    private suspend fun start(config: String, mesh: MeshInfo): Boolean = withContext(CoreDispatcher) {
        try {
            // Port 0: the port in the config, or the core's default.
            val started = Mobile.startRelay(config, Uplinks.cellular.name, "", 0, BondPlatform)
            relay = started
            advertiser.announce(mesh.name, started.port(), mesh.relayTxt)
            publish(started)
            true
        } catch (e: Exception) {
            Log.w(TAG, "cannot start the relay", e)
            BondPlatform.releaseLan()
            show(RelayState.Waiting(e.message ?: e.toString()), getString(R.string.share_retrying))
            false
        }
    }

    /**
     * Tells the core when Android has replaced a network.
     *
     * The core binds each socket to a network once, when it opens it. When mobile data is replaced, or
     * comes back, its per-hub sockets have to be opened afresh. The hub-facing socket lives as long as
     * the relay does, so that one is moved to the new Wi-Fi network instead.
     */
    private suspend fun followNetworks() {
        var shared: Network? = null
        combine(Uplinks.cellular.network, Uplinks.wifi.network, ::Pair).collect { (cellular, _) ->
            BondPlatform.rebindLan()
            withContext(CoreDispatcher) {
                val running = relay ?: return@withContext
                if (cellular != null && cellular != shared) running.networksChanged()
                publish(running)
            }
            shared = cellular
        }
    }

    private fun close() {
        relay?.stop()
        relay = null
        BondPlatform.releaseLan()
    }

    /** Runs on [CoreDispatcher], like everything that sets the state while the session is alive. */
    private fun publish(relay: Relay) {
        val status = RelayStatus.parse(relay.status())
        if (Uplinks.cellular.network.value == null) {
            // The relay stays up, and stays announced, while mobile data is away: the core simply has
            // nowhere to forward to, and picks up again by itself.
            show(RelayState.Waiting(), getString(R.string.share_waiting))
            return
        }
        val hubs = resources.getQuantityString(R.plurals.share_hubs, status.clients, status.clients)
        val forwarded = Formatter.formatShortFileSize(this, status.forwardedBytes)
        show(
            RelayState.Running(status, advertiser.state.value, lanAddress(status.port)),
            getString(R.string.share_summary, hubs, forwarded),
        )
    }

    private fun show(state: RelayState, text: String) {
        RelayMonitor.state.value = state
        if (text == notified) return
        notified = text
        Notifications.update(this, Notifications.RELAY_ID, notification(text))
    }

    private fun lanAddress(port: Int): String? {
        val wifi = Uplinks.wifi.network.value ?: return null
        val links = getSystemService(ConnectivityManager::class.java).getLinkProperties(wifi) ?: return null
        val ip = links.linkAddresses.firstOrNull { it.address is Inet4Address }?.address?.hostAddress
        return ip?.let { "$it:$port" }
    }

    private fun notification(text: String) = Notifications.status(
        this,
        title = getString(R.string.mode_share),
        text = text,
        stopLabel = getString(R.string.action_stop),
        stopAction = ACTION_STOP,
    )

    // Held for as long as sharing is on, by design: a relay that dozes drops the hub's path.
    @SuppressLint("WakelockTimeout")
    private fun acquireLocks() {
        wakeLock = getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "bond:relay")
            .apply { acquire() }
        // Keeps Wi-Fi out of power save, which otherwise adds tens of milliseconds to every packet
        // from the hub. High-perf is the lock that works with the screen off; Android 14 folded it
        // into low-latency.
        @Suppress("DEPRECATION")
        val mode = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            WifiManager.WIFI_MODE_FULL_LOW_LATENCY
        } else {
            WifiManager.WIFI_MODE_FULL_HIGH_PERF
        }
        wifiLock = applicationContext.getSystemService(WifiManager::class.java)
            .createWifiLock(mode, "bond:relay")
            .apply { acquire() }
    }

    private fun releaseLocks() {
        wifiLock?.takeIf { it.isHeld }?.release()
        wakeLock?.takeIf { it.isHeld }?.release()
        wifiLock = null
        wakeLock = null
    }

    companion object {
        private const val TAG = "BondRelay"
        private const val ACTION_STOP = "com.cartertech.bond.relay.STOP"
        private const val RETRY_MS = 10_000L

        @Volatile
        private var running: RelayService? = null

        /**
         * Switches sharing on. Returns false if Android would not let the service start, which it may
         * do when the app is in the background and has not been exempted from battery optimisation.
         */
        fun start(context: Context): Boolean {
            return try {
                ContextCompat.startForegroundService(context, Intent(context, RelayService::class.java))
                true
            } catch (e: IllegalStateException) {
                Log.w(TAG, "cannot start sharing from the background", e)
                false
            }
        }

        fun stop(context: Context) {
            context.bond.prefs.shareEnabled = false
            running?.shutDown()
        }
    }
}
