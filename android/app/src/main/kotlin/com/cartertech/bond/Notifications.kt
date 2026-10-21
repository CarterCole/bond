package com.cartertech.bond

import android.annotation.SuppressLint
import android.app.Notification
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat

/** The two ongoing notifications: one per foreground service. */
object Notifications {
    const val VPN_ID = 1
    const val RELAY_ID = 2
    private const val CHANNEL = "status"

    fun createChannel(context: Context) {
        val channel = NotificationChannelCompat.Builder(CHANNEL, NotificationManagerCompat.IMPORTANCE_LOW)
            .setName(context.getString(R.string.channel_status))
            .setShowBadge(false)
            .build()
        NotificationManagerCompat.from(context).createNotificationChannel(channel)
    }

    fun openApp(context: Context): PendingIntent = PendingIntent.getActivity(
        context,
        0,
        Intent(context, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    /** An ongoing notification with one button, which sends [stopAction] to [service]. */
    fun status(
        service: Service,
        title: String,
        text: String,
        stopLabel: String,
        stopAction: String,
    ): Notification {
        val stop = PendingIntent.getService(
            service,
            0,
            Intent(service, service.javaClass).setAction(stopAction),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return NotificationCompat.Builder(service, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_bond)
            .setContentTitle(title)
            .setContentText(text)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .setContentIntent(openApp(service))
            .addAction(0, stopLabel, stop)
            .build()
    }

    // Without the notification permission the service still runs; there is just nothing to update.
    @SuppressLint("MissingPermission")
    fun update(context: Context, id: Int, notification: Notification) {
        val manager = NotificationManagerCompat.from(context)
        if (manager.areNotificationsEnabled()) manager.notify(id, notification)
    }
}
