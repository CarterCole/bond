package com.cartertech.bond

import android.app.Application
import android.content.Context
import com.cartertech.bond.core.ConfigStore
import com.cartertech.bond.core.Prefs
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch

class BondApp : Application() {
    lateinit var config: ConfigStore
        private set
    lateinit var prefs: Prefs
        private set

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onCreate() {
        super.onCreate()
        config = ConfigStore(this)
        prefs = Prefs(this)
        Notifications.createChannel(this)
        scope.launch { config.load() }
    }
}

val Context.bond: BondApp get() = applicationContext as BondApp
