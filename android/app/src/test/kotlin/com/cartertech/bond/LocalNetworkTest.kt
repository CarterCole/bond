package com.cartertech.bond

import com.cartertech.bond.core.LocalNetwork
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class LocalNetworkTest {
    @Test
    fun hostOfJoinCode() {
        assertEquals("203.0.113.5", LocalNetwork.hostOfJoinCode("bond://203.0.113.5:7777/MFRGGZDF"))
        assertEquals("bond.example.com", LocalNetwork.hostOfJoinCode("  bond://bond.example.com:7777/MFRGGZDF\n"))
        assertEquals("[fd00::1]", LocalNetwork.hostOfJoinCode("bond://[fd00::1]:7777/MFRGGZDF"))
        assertNull(LocalNetwork.hostOfJoinCode("not a code"))
    }

    @Test
    fun hostOfAggregator() {
        assertEquals("192.168.1.10", LocalNetwork.hostOf("192.168.1.10:7777"))
        assertEquals("[fd00::1]", LocalNetwork.hostOf("[fd00::1]:7777"))
    }

    @Test
    fun localAddresses() {
        for (host in listOf("192.168.1.10", "10.0.2.2", "172.16.5.1", "169.254.1.1", "127.0.0.1", "100.64.0.9",
            "[fd00::1]", "fe80::1", "nas.local")) {
            assertTrue(host, LocalNetwork.isLocal(host))
        }
    }

    @Test
    fun publicAddresses() {
        // A DNS name is never resolved here, so it counts as not local.
        for (host in listOf("203.0.113.5", "1.1.1.1", "100.128.0.1", "172.32.0.1", "[2001:db8::1]",
            "bond.example.com", "")) {
            assertFalse(host, LocalNetwork.isLocal(host))
        }
    }
}
