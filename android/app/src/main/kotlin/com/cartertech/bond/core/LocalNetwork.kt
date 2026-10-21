package com.cartertech.bond.core

import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import java.net.Inet6Address
import java.net.InetAddress
import java.net.URI

/**
 * Android 17 (API 37) puts the local network behind a runtime permission: without it, sockets to and
 * from LAN addresses fail and NsdManager refuses to announce. Sharing always needs it (hubs reach the
 * phone over Wi-Fi); bonding needs it only when the aggregator itself is on the LAN.
 */
object LocalNetwork {
    const val PERMISSION = "android.permission.ACCESS_LOCAL_NETWORK"
    private const val ANDROID_17 = 37

    fun permissionMissing(context: Context): Boolean =
        Build.VERSION.SDK_INT >= ANDROID_17 &&
            context.checkSelfPermission(PERMISSION) != PackageManager.PERMISSION_GRANTED

    /** The host of a join code: bond://host:port/secret. */
    fun hostOfJoinCode(code: String): String? = runCatching { URI(code.trim()).host }.getOrNull()

    /** The host of "host:port". */
    fun hostOf(hostPort: String): String = hostPort.substringBeforeLast(':')

    /** True for an address on the local network. A DNS name cannot be judged without resolving it. */
    fun isLocal(host: String): Boolean {
        val h = host.removePrefix("[").removeSuffix("]")
        if (h.endsWith(".local", ignoreCase = true)) return true
        // Only literals: getByName would otherwise go and resolve the name.
        if (!IPV4.matches(h) && ':' !in h) return false
        val address = runCatching { InetAddress.getByName(h) }.getOrNull() ?: return false
        return address.isSiteLocalAddress || address.isLinkLocalAddress || address.isLoopbackAddress ||
            address.isUniqueLocal() || address.isCarrierGradeNat()
    }

    private val IPV4 = Regex("""\d{1,3}(\.\d{1,3}){3}""")

    private fun InetAddress.isUniqueLocal() = this is Inet6Address && (address[0].toInt() and 0xfe) == 0xfc

    // 100.64.0.0/10: what some hotspots and overlay networks hand out.
    private fun InetAddress.isCarrierGradeNat(): Boolean {
        val b = address
        return b.size == 4 && (b[0].toInt() and 0xff) == 100 && (b[1].toInt() and 0xc0) == 64
    }
}
