package com.cartertech.bond.ui

import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.cartertech.bond.core.MeshState

@Composable
fun BondRoot(viewModel: BondViewModel = viewModel()) {
    val mesh by viewModel.mesh.collectAsStateWithLifecycle()
    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
        when (val state = mesh) {
            MeshState.Loading -> Unit // reading one small file: not worth a spinner
            MeshState.None -> JoinScreen(viewModel)
            is MeshState.Joined -> MainScreen(state.info, viewModel)
        }
    }
}
