package com.cartertech.bond.core

import android.content.Context
import android.util.AtomicFile
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import mobile.Mobile
import java.io.File
import java.io.FileNotFoundException

sealed interface MeshState {
    data object Loading : MeshState
    data object None : MeshState
    data class Joined(val info: MeshInfo) : MeshState
}

/**
 * The device config that Mobile.join() returned. It holds the mesh's private keys, so it lives in
 * noBackupFilesDir, which Android leaves out of backups and device transfers whatever the manifest says.
 *
 * Every method does file I/O: call them off the main thread.
 */
class ConfigStore(context: Context) {
    private val file = AtomicFile(File(context.noBackupFilesDir, "config.json"))
    private val _mesh = MutableStateFlow<MeshState>(MeshState.Loading)
    val mesh: StateFlow<MeshState> = _mesh

    @Synchronized
    fun read(): String? = try {
        file.readFully().decodeToString()
    } catch (_: FileNotFoundException) {
        null
    }

    /**
     * The config with the aggregator's address looked up afresh, which is what lets the core connect
     * when it cannot use DNS (inside its own tunnel, or between networks). Call it just before starting
     * the client or the relay; it can take a few seconds.
     */
    fun refreshed(): String? {
        val stored = read() ?: return null
        val fresh = Mobile.refresh(stored) // never fails: unchanged if the lookup did not work
        if (fresh != stored) {
            synchronized(this) {
                // Unless settings were saved, or the mesh left, while the lookup ran.
                if (read() == stored) write(fresh)
            }
        }
        return fresh
    }

    @Synchronized
    fun load() {
        _mesh.value = describe(read())
    }

    /** Stores the config of a mesh just joined. */
    @Synchronized
    fun save(json: String) {
        write(json)
        _mesh.value = describe(json)
    }

    /**
     * Stores settings the running core changed (path policies, priority). Does nothing once the mesh
     * has been left, so a service that is still shutting down cannot bring the config back.
     */
    @Synchronized
    fun update(json: String) {
        if (file.baseFile.exists()) write(json)
    }

    @Synchronized
    fun clear() {
        file.delete()
        _mesh.value = MeshState.None
    }

    private fun write(json: String) {
        val out = file.startWrite()
        try {
            out.write(json.encodeToByteArray())
            file.finishWrite(out)
        } catch (e: Exception) {
            file.failWrite(out)
            throw e
        }
    }

    // A config the core cannot read is as good as none: the join screen replaces it.
    private fun describe(json: String?): MeshState {
        val info = json?.let { runCatching { MeshInfo.parse(Mobile.describe(it)) }.getOrNull() }
        return if (info == null) MeshState.None else MeshState.Joined(info)
    }
}
