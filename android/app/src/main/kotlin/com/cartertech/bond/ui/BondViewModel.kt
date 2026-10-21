package com.cartertech.bond.ui

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.cartertech.bond.BondApp
import com.cartertech.bond.R
import com.cartertech.bond.core.MeshState
import com.cartertech.bond.core.Policy
import com.cartertech.bond.relay.RelayMonitor
import com.cartertech.bond.relay.RelayService
import com.cartertech.bond.relay.RelayState
import com.cartertech.bond.vpn.BondVpnService
import com.cartertech.bond.vpn.VpnMonitor
import com.cartertech.bond.vpn.VpnState
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.receiveAsFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import mobile.Mobile

/** The two things the phone can do for a mesh. */
enum class Mode { Bond, Share }

data class JoinState(val busy: Boolean = false, val error: String? = null)

class BondViewModel(app: Application) : AndroidViewModel(app) {
    private val bond = app as BondApp

    val mesh: StateFlow<MeshState> = bond.config.mesh
    val vpn: StateFlow<VpnState> = VpnMonitor.state
    val relay: StateFlow<RelayState> = RelayMonitor.state

    private val _join = MutableStateFlow(JoinState())
    val join: StateFlow<JoinState> = _join

    private val _coreVersion = MutableStateFlow("")
    val coreVersion: StateFlow<String> = _coreVersion

    private val _messages = Channel<String>(Channel.BUFFERED)

    /** Things to tell the user once, in passing. */
    val messages: Flow<String> = _messages.receiveAsFlow()

    init {
        viewModelScope.launch(Dispatchers.IO) { _coreVersion.value = Mobile.version() }
        // Sharing stays on until it is switched off. If Android stopped the app in the meantime (a
        // force stop, say), nothing restarts it until the app is opened: pick it up again here.
        if (bond.prefs.shareEnabled && relay.value is RelayState.Off) RelayService.start(bond)
    }

    fun join(code: String, deviceName: String) {
        if (_join.value.busy) return
        _join.value = JoinState(busy = true)
        viewModelScope.launch {
            val error = try {
                // Talks to the aggregator: up to 20 seconds.
                withContext(Dispatchers.IO) { bond.config.save(Mobile.join(code.trim(), deviceName.trim(), "phone")) }
                null
            } catch (e: Exception) {
                // The core words its errors for the person joining.
                e.message ?: e.toString()
            }
            _join.value = JoinState(error = error)
        }
    }

    fun clearJoinError() {
        if (!_join.value.busy) _join.value = JoinState()
    }

    /** Call once Android's prompts for [mode] have been answered: see rememberModeStarter. */
    fun start(mode: Mode) {
        val started = when (mode) {
            Mode.Bond -> runCatching { BondVpnService.start(bond) }.isSuccess
            Mode.Share -> RelayService.start(bond)
        }
        if (!started) report(bond.getString(R.string.error_cannot_start))
    }

    fun stop(mode: Mode) = when (mode) {
        Mode.Bond -> BondVpnService.stop()
        Mode.Share -> RelayService.stop(bond)
    }

    fun setPolicy(path: String, policy: Policy) = change { BondVpnService.setPolicy(path, policy.wire) }

    fun setPriority(all: Boolean) = change { BondVpnService.setPriority(all) }

    fun leave() {
        stop(Mode.Bond)
        stop(Mode.Share)
        viewModelScope.launch(Dispatchers.IO) { bond.config.clear() }
    }

    fun report(message: String) {
        _messages.trySend(message)
    }

    private fun change(block: suspend () -> Unit) {
        viewModelScope.launch {
            try {
                block()
            } catch (e: Exception) {
                report(e.message ?: e.toString())
            }
        }
    }
}
