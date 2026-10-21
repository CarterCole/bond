package com.cartertech.bond.core

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Where the Go core is called from.
 *
 * One call at a time, for the whole process: the VPN client and the relay can be starting, stopping and
 * being polled at once (and a service that is going away can overlap its own replacement), and none of
 * that should interleave. It also keeps every call off the main thread.
 */
val CoreDispatcher: CoroutineDispatcher = Dispatchers.IO.limitedParallelism(1)

private const val WATCHED_POLL_MS = 1_000L
private const val UNWATCHED_POLL_MS = 5_000L

/**
 * Waits until status should be read again: a second while a screen is showing it, otherwise long enough
 * to keep the notification honest without waking the phone for nobody. Returns at once when a screen
 * starts watching.
 */
suspend fun awaitNextPoll(watchers: StateFlow<Int>) {
    if (watchers.value > 0) {
        delay(WATCHED_POLL_MS)
    } else {
        withTimeoutOrNull(UNWATCHED_POLL_MS) { watchers.first { it > 0 } }
    }
}
