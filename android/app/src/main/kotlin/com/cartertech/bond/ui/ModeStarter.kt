package com.cartertech.bond.ui

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.core.net.toUri
import com.cartertech.bond.R
import com.cartertech.bond.bond
import com.cartertech.bond.core.LocalNetwork

/**
 * Returns a function that switches a mode on, first walking the user through whatever Android wants
 * agreed to, one prompt at a time:
 *
 *  - notifications (asked once; the modes work without, but their notification is how to stop them)
 *  - the local network (Android 17; sharing cannot work without it)
 *  - an exemption from battery optimisation, for sharing (asked once)
 *  - the system's VPN consent, for bonding
 *
 * Each answer comes back here and the walk carries on from the top; steps already settled are skipped.
 */
@Composable
fun rememberModeStarter(
    aggregator: String,
    onReady: (Mode) -> Unit,
    onMessage: (String) -> Unit,
): (Mode) -> Unit {
    val context = LocalContext.current
    val prefs = remember { context.bond.prefs }
    val localNetworkDenied = stringResource(R.string.local_network_denied)
    val vpnDenied = stringResource(R.string.vpn_denied)
    var pending by rememberSaveable { mutableStateOf<Mode?>(null) }
    var askingBattery by rememberSaveable { mutableStateOf(false) }

    // The launchers' callbacks and the walk refer to each other; this breaks the circle.
    val walk = remember { arrayOf<(Mode) -> Unit>({}) }
    fun resume() {
        pending?.let { walk[0](it) }
    }

    fun giveUp(why: String) {
        pending = null
        onMessage(why)
    }

    val notifications = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        resume()
    }
    val localNetwork = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) resume() else giveUp(localNetworkDenied)
    }
    val vpnConsent = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        if (result.resultCode == Activity.RESULT_OK) resume() else giveUp(vpnDenied)
    }
    val batteryExemption = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) {
        resume()
    }

    walk[0] = { mode ->
        pending = mode
        val needsLan = mode == Mode.Share || LocalNetwork.isLocal(LocalNetwork.hostOf(aggregator))
        val consent = if (mode == Mode.Bond) VpnService.prepare(context) else null
        when {
            context.shouldAskForNotifications() && !prefs.askedNotifications -> {
                prefs.askedNotifications = true
                notifications.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
            needsLan && LocalNetwork.permissionMissing(context) -> localNetwork.launch(LocalNetwork.PERMISSION)
            mode == Mode.Share && !context.isExemptFromBatteryOptimisation() && !prefs.askedBattery -> {
                prefs.askedBattery = true
                askingBattery = true
            }
            consent != null -> vpnConsent.launch(consent)
            else -> {
                pending = null
                onReady(mode)
            }
        }
    }

    if (askingBattery) {
        AlertDialog(
            onDismissRequest = {
                askingBattery = false
                resume()
            },
            title = { Text(stringResource(R.string.battery_title)) },
            text = { Text(stringResource(R.string.battery_text)) },
            confirmButton = {
                TextButton(onClick = {
                    askingBattery = false
                    try {
                        batteryExemption.launch(context.batteryExemptionRequest())
                    } catch (_: ActivityNotFoundException) {
                        resume()
                    }
                }) { Text(stringResource(R.string.battery_allow)) }
            },
            dismissButton = {
                TextButton(onClick = {
                    askingBattery = false
                    resume()
                }) { Text(stringResource(R.string.battery_skip)) }
            },
        )
    }

    return { mode -> walk[0](mode) }
}

private fun Context.shouldAskForNotifications(): Boolean =
    Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
        checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED

private fun Context.isExemptFromBatteryOptimisation(): Boolean =
    getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)

// A relay has to answer hubs for hours with the screen off, which is the case this request exists for.
@SuppressLint("BatteryLife")
private fun Context.batteryExemptionRequest(): Intent =
    Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, "package:$packageName".toUri())
