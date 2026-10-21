package com.cartertech.bond.relay

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import com.cartertech.bond.bond

/** Sharing is meant to be left on: bring it back after a reboot or an update of the app. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED, Intent.ACTION_MY_PACKAGE_REPLACED ->
                if (context.bond.prefs.shareEnabled) RelayService.start(context)
        }
    }
}
