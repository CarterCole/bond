package com.cartertech.bond

import com.cartertech.bond.core.ClientStatus
import com.cartertech.bond.core.MeshInfo
import com.cartertech.bond.core.RelayStatus
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** The JSON documents here are what the Go core produces: see core/mobile, core/node and core/bond. */
class ModelsTest {
    @Test
    fun describe() {
        val mesh = MeshInfo.parse(
            """{"name":"Pixel 9","kind":"phone","mesh_id":"ae519f22e03d","address":"10.207.0.5","prefix":16,
               "gateway":"10.207.0.1","aggregator":"203.0.113.5:7777","mtu":1280,"dns":["1.1.1.1","1.0.0.1"],
               "relay_txt":["v=1","mesh=ae519f22e03d","name=Pixel 9"]}""",
        )
        assertEquals("Pixel 9", mesh.name)
        assertEquals("10.207.0.5", mesh.address)
        assertEquals(16, mesh.prefix)
        assertEquals(1280, mesh.mtu)
        assertEquals(listOf("1.1.1.1", "1.0.0.1"), mesh.dns)
        assertEquals(listOf("v=1", "mesh=ae519f22e03d", "name=Pixel 9"), mesh.relayTxt)
    }

    @Test
    fun describeWithoutDns() {
        // Go writes an empty list as null.
        val mesh = MeshInfo.parse("""{"name":"p","address":"10.207.0.5","prefix":16,"dns":null,"relay_txt":null}""")
        assertTrue(mesh.dns.isEmpty())
        assertEquals(1280, mesh.mtu)
    }

    @Test
    fun clientStatus() {
        val status = ClientStatus.parse(
            """{"role":"hub","name":"Pixel 9","mode":"vpn","address":"10.207.0.5","aggregator":"203.0.113.5:7777",
               "connected":true,"handshake_age_sec":12,"uptime_sec":340,"realtime_packets":7,"priority":{"all":true},
               "session":{"reorder_hold_ms":5,"paths":[
                 {"id":0,"name":"cellular","kind":"direct","policy":"bonded","up":true,"rtt_ms":41.5,"loss":0.012,
                  "capacity_bps":0,"tx_bps":120000,"rx_bps":2400000,"tx_bytes":1000,"rx_bytes":5000000000},
                 {"id":1,"name":"wifi","kind":"direct","policy":"primary","up":false,"rtt_ms":0,"loss":0,
                  "capacity_bps":0,"tx_bps":0,"rx_bps":0,"tx_bytes":0,"rx_bytes":0}]}}""",
        )
        assertTrue(status.connected)
        assertTrue(status.priorityAll)
        assertEquals(340, status.uptimeSec)
        assertEquals(listOf("cellular", "wifi"), status.paths.map { it.name })
        val cellular = status.paths[0]
        assertEquals("bonded", cellular.policy)
        assertTrue(cellular.up)
        assertEquals(41.5, cellular.rttMs, 0.0)
        assertEquals(0.012, cellular.loss, 0.0)
        assertEquals(5_000_000_000, cellular.rxBytes)
        assertFalse(status.paths[1].up)
    }

    @Test
    fun clientStatusBeforeAnything() {
        // No paths yet, never connected, and "all" left out because it is false.
        val status = ClientStatus.parse(
            """{"role":"hub","connected":false,"handshake_age_sec":-1,"priority":{},"session":{"paths":null}}""",
        )
        assertFalse(status.connected)
        assertFalse(status.priorityAll)
        assertEquals(-1, status.handshakeAgeSec)
        assertTrue(status.paths.isEmpty())
    }

    @Test
    fun relayStatus() {
        val status = RelayStatus.parse(
            """{"role":"relay","name":"Pixel 9","uptime_sec":60,"relay":{"clients":1,"up_bytes":245000,
               "down_bytes":3300000,"up_packets":900,"down_packets":2600,"refused":2,"port":7778,
               "upstream":"203.0.113.5:7777"}}""",
        )
        assertEquals(1, status.clients)
        assertEquals(7778, status.port)
        assertEquals(3_545_000, status.forwardedBytes)
        assertEquals(2, status.refused)
        assertEquals("203.0.113.5:7777", status.upstream)
    }
}
