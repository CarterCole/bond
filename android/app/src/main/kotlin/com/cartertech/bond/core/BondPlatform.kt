package com.cartertech.bond.core

import android.net.Network
import android.os.ParcelFileDescriptor
import android.util.Log
import com.cartertech.bond.vpn.BondVpnService
import mobile.Platform
import java.io.IOException

/**
 * What the Go core asks of Android, for the VPN client and the relay alike. Called from Go's threads,
 * for every socket the core opens, before the socket is used.
 */
object BondPlatform : Platform {
    private const val TAG = "BondCore"

    /**
     * Our own handle on the relay's hub-facing socket. The core opens that socket once and keeps it, so
     * when Android replaces the Wi-Fi network it is this handle that lets the socket be moved across.
     */
    private var lan: ParcelFileDescriptor? = null

    override fun bindSocket(fd: Int, network: String): Boolean {
        // Keep the socket out of our own tunnel. Without a VPN there is nothing to be kept out of.
        val vpn = BondVpnService.running
        if (vpn != null && !vpn.protect(fd)) Log.w(TAG, "could not protect a socket for \"$network\"")

        return when (network) {
            "" -> true
            "lan" -> {
                holdLan(fd)
                true
            }
            // False means "that network is not there": the core then does without the socket rather
            // than let it leave by whatever the default network is.
            else -> Uplinks.named(network)?.network?.value?.let { attach(fd, it, network) } ?: false
        }
    }

    override fun log(line: String) {
        Log.i(TAG, line)
    }

    /**
     * Puts the relay's hub-facing socket on the current Wi-Fi network. With no Wi-Fi it stays where it
     * was (at first, on the default network); call this again when Wi-Fi arrives or is replaced.
     */
    @Synchronized
    fun rebindLan() {
        val socket = lan ?: return
        val wifi = Uplinks.wifi.network.value ?: return
        try {
            wifi.bindSocket(socket.fileDescriptor)
        } catch (e: IOException) {
            Log.w(TAG, "could not move the hub-facing socket to Wi-Fi", e)
        }
    }

    /** Call once the relay has stopped: until then our handle keeps its port taken. */
    @Synchronized
    fun releaseLan() {
        lan?.close()
        lan = null
    }

    @Synchronized
    private fun holdLan(fd: Int) {
        lan?.close()
        lan = try {
            ParcelFileDescriptor.fromFd(fd)
        } catch (e: IOException) {
            Log.w(TAG, "could not keep a handle on the hub-facing socket", e)
            null
        }
        rebindLan()
    }

    private fun attach(fd: Int, target: Network, name: String): Boolean = try {
        // The descriptor is Go's. fromFd duplicates it; binding the duplicate binds the same socket.
        ParcelFileDescriptor.fromFd(fd).use { target.bindSocket(it.fileDescriptor) }
        true
    } catch (e: IOException) {
        Log.w(TAG, "could not bind a socket to \"$name\"", e)
        false
    }
}
