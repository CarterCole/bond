package com.cartertech.bond.ui

import java.util.Locale

// The same units and rounding as the desktop dashboard, so the two can be compared side by side.

/** A rate in bits per second. */
fun formatRate(bps: Double): String = when {
    bps >= 1e9 -> String.format(Locale.getDefault(), "%.2f Gbit/s", bps / 1e9)
    bps >= 1e6 -> String.format(Locale.getDefault(), "%.1f Mbit/s", bps / 1e6)
    bps >= 1e3 -> String.format(Locale.getDefault(), "%.0f kbit/s", bps / 1e3)
    else -> String.format(Locale.getDefault(), "%.0f bit/s", bps)
}

/** A loss fraction (0..1) as a percentage. */
fun formatLoss(loss: Double): String = String.format(Locale.getDefault(), "%.1f %%", loss * 100)

fun formatDuration(seconds: Long): String = when {
    seconds >= 86_400 -> "${seconds / 86_400}d ${seconds % 86_400 / 3_600}h"
    seconds >= 3_600 -> "${seconds / 3_600}h ${seconds % 3_600 / 60}m"
    seconds >= 60 -> "${seconds / 60}m"
    else -> "${seconds}s"
}
