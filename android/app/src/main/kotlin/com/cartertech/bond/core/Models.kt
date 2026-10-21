package com.cartertech.bond.core

import org.json.JSONArray
import org.json.JSONObject

/** What Mobile.describe() says about a config: enough to build the tunnel and announce a relay. */
data class MeshInfo(
    val name: String,
    val meshId: String,
    val address: String,
    val prefix: Int,
    val gateway: String,
    val aggregator: String,
    val mtu: Int,
    val dns: List<String>,
    val relayTxt: List<String>,
) {
    companion object {
        fun parse(json: String): MeshInfo {
            val o = JSONObject(json)
            return MeshInfo(
                name = o.optString("name"),
                meshId = o.optString("mesh_id"),
                address = o.getString("address"),
                prefix = o.getInt("prefix"),
                gateway = o.optString("gateway"),
                aggregator = o.optString("aggregator"),
                mtu = o.optInt("mtu", 1280),
                dns = o.optJSONArray("dns").strings(),
                relayTxt = o.optJSONArray("relay_txt").strings(),
            )
        }
    }
}

enum class Policy(val wire: String) {
    Primary("primary"),
    Bonded("bonded"),
    Backup("backup"),
    Off("off"),
}

data class PathStatus(
    val name: String,
    val kind: String,
    val policy: String,
    val up: Boolean,
    val rttMs: Double,
    /** Fraction of packets lost, 0..1. */
    val loss: Double,
    /** Bits per second. */
    val txBps: Double,
    val rxBps: Double,
    val txBytes: Long,
    val rxBytes: Long,
)

/** Client.status(). */
data class ClientStatus(
    val address: String,
    val aggregator: String,
    val connected: Boolean,
    /** Seconds since the last WireGuard handshake; -1 before the first. */
    val handshakeAgeSec: Long,
    val uptimeSec: Long,
    val priorityAll: Boolean,
    val paths: List<PathStatus>,
) {
    companion object {
        fun parse(json: String): ClientStatus {
            val o = JSONObject(json)
            val paths = o.optJSONObject("session")?.optJSONArray("paths")
            return ClientStatus(
                address = o.optString("address"),
                aggregator = o.optString("aggregator"),
                connected = o.optBoolean("connected"),
                handshakeAgeSec = o.optLong("handshake_age_sec", -1),
                uptimeSec = o.optLong("uptime_sec"),
                priorityAll = o.optJSONObject("priority")?.optBoolean("all") ?: false,
                paths = paths.objects().map { p ->
                    PathStatus(
                        name = p.optString("name"),
                        kind = p.optString("kind"),
                        policy = p.optString("policy"),
                        up = p.optBoolean("up"),
                        rttMs = p.optDouble("rtt_ms", 0.0),
                        loss = p.optDouble("loss", 0.0),
                        txBps = p.optDouble("tx_bps", 0.0),
                        rxBps = p.optDouble("rx_bps", 0.0),
                        txBytes = p.optLong("tx_bytes"),
                        rxBytes = p.optLong("rx_bytes"),
                    )
                },
            )
        }
    }
}

/** Relay.status(). Up is hub → aggregator, down is aggregator → hub. */
data class RelayStatus(
    val uptimeSec: Long,
    val clients: Int,
    val upBytes: Long,
    val downBytes: Long,
    val upPackets: Long,
    val downPackets: Long,
    val refused: Long,
    val port: Int,
    val upstream: String,
) {
    val forwardedBytes: Long get() = upBytes + downBytes

    companion object {
        fun parse(json: String): RelayStatus {
            val o = JSONObject(json)
            val r = o.optJSONObject("relay") ?: JSONObject()
            return RelayStatus(
                uptimeSec = o.optLong("uptime_sec"),
                clients = r.optInt("clients"),
                upBytes = r.optLong("up_bytes"),
                downBytes = r.optLong("down_bytes"),
                upPackets = r.optLong("up_packets"),
                downPackets = r.optLong("down_packets"),
                refused = r.optLong("refused"),
                port = r.optInt("port"),
                upstream = r.optString("upstream"),
            )
        }
    }
}

// Go writes an empty list as null, so every array is optional.
private fun JSONArray?.strings(): List<String> =
    if (this == null) emptyList() else List(length()) { getString(it) }

private fun JSONArray?.objects(): List<JSONObject> =
    if (this == null) emptyList() else List(length()) { getJSONObject(it) }
