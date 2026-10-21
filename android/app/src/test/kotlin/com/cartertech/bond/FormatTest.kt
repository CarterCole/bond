package com.cartertech.bond

import com.cartertech.bond.ui.formatDuration
import com.cartertech.bond.ui.formatLoss
import com.cartertech.bond.ui.formatRate
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Before
import org.junit.Test
import java.util.Locale

class FormatTest {
    private val locale = Locale.getDefault()

    @Before
    fun useEnglish() = Locale.setDefault(Locale.US)

    @After
    fun restore() = Locale.setDefault(locale)

    @Test
    fun rates() {
        assertEquals("0 bit/s", formatRate(0.0))
        assertEquals("850 kbit/s", formatRate(850_000.0))
        assertEquals("12.4 Mbit/s", formatRate(12_400_000.0))
        assertEquals("1.25 Gbit/s", formatRate(1_250_000_000.0))
    }

    @Test
    fun loss() {
        assertEquals("0.0 %", formatLoss(0.0))
        assertEquals("1.2 %", formatLoss(0.012))
    }

    @Test
    fun durations() {
        assertEquals("42s", formatDuration(42))
        assertEquals("5m", formatDuration(310))
        assertEquals("1h 5m", formatDuration(3_900))
        assertEquals("2d 3h", formatDuration(2 * 86_400 + 3 * 3_600 + 59))
    }
}
