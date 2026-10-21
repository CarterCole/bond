package com.cartertech.bond.core

import android.content.Context
import androidx.core.content.edit

class Prefs(context: Context) {
    private val prefs = context.getSharedPreferences("bond", Context.MODE_PRIVATE)

    /** The user has sharing switched on: it is started again after a reboot. */
    var shareEnabled: Boolean
        get() = prefs.getBoolean(SHARE, false)
        set(value) = prefs.edit { putBoolean(SHARE, value) }

    /** Each of these is asked for once; after that the answer is the user's to change in Settings. */
    var askedNotifications: Boolean
        get() = prefs.getBoolean(ASKED_NOTIFICATIONS, false)
        set(value) = prefs.edit { putBoolean(ASKED_NOTIFICATIONS, value) }

    var askedBattery: Boolean
        get() = prefs.getBoolean(ASKED_BATTERY, false)
        set(value) = prefs.edit { putBoolean(ASKED_BATTERY, value) }

    private companion object {
        const val SHARE = "share_enabled"
        const val ASKED_NOTIFICATIONS = "asked_notifications"
        const val ASKED_BATTERY = "asked_battery"
    }
}
