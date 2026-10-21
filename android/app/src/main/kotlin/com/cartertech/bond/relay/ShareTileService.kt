package com.cartertech.bond.relay

import android.content.Intent
import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import com.cartertech.bond.MainActivity
import com.cartertech.bond.Notifications
import com.cartertech.bond.R
import com.cartertech.bond.bond
import com.cartertech.bond.core.LocalNetwork
import com.cartertech.bond.core.MeshState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch

/** A Quick Settings tile for "Share my connection". */
class ShareTileService : TileService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watcher: Job? = null

    override fun onStartListening() {
        watcher?.cancel()
        watcher = scope.launch { RelayMonitor.state.collect(::render) }
    }

    override fun onStopListening() {
        watcher?.cancel()
        watcher = null
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    override fun onClick() {
        when {
            RelayMonitor.state.value !is RelayState.Off -> RelayService.stop(this)
            // Joining a mesh and granting permissions need the app's screens.
            bond.config.mesh.value is MeshState.None || LocalNetwork.permissionMissing(this) -> openApp()
            !RelayService.start(this) -> openApp()
        }
    }

    private fun render(state: RelayState) {
        val tile = qsTile ?: return
        tile.state = if (state is RelayState.Off) Tile.STATE_INACTIVE else Tile.STATE_ACTIVE
        tile.subtitle = when (state) {
            is RelayState.Off -> null
            is RelayState.Starting -> getString(R.string.share_starting)
            is RelayState.Waiting -> getString(R.string.share_waiting)
            is RelayState.Running ->
                resources.getQuantityString(R.plurals.share_hubs, state.status.clients, state.status.clients)
        }
        tile.updateTile()
    }

    // Asks for the screen to be unlocked first, if it is locked.
    private fun openApp() = unlockAndRun {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startActivityAndCollapse(Notifications.openApp(this))
        } else {
            @Suppress("DEPRECATION", "StartActivityAndCollapseDeprecated")
            startActivityAndCollapse(
                Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            )
        }
    }
}
