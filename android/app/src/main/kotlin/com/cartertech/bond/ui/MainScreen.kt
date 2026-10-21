package com.cartertech.bond.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.cartertech.bond.R
import com.cartertech.bond.core.MeshInfo

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MainScreen(mesh: MeshInfo, viewModel: BondViewModel) {
    val vpn by viewModel.vpn.collectAsStateWithLifecycle()
    val relay by viewModel.relay.collectAsStateWithLifecycle()
    val coreVersion by viewModel.coreVersion.collectAsStateWithLifecycle()
    val snackbar = remember { SnackbarHostState() }
    var confirmLeave by rememberSaveable { mutableStateOf(false) }

    LaunchedEffect(viewModel) {
        viewModel.messages.collect { snackbar.showSnackbar(it) }
    }
    val start = rememberModeStarter(mesh.aggregator, onReady = viewModel::start, onMessage = viewModel::report)

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text(stringResource(R.string.app_name))
                        Text(mesh.name, style = MaterialTheme.typography.bodySmall)
                    }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbar) },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp, vertical = 8.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            BondCard(
                state = vpn,
                onToggle = { on -> if (on) start(Mode.Bond) else viewModel.stop(Mode.Bond) },
                onPolicy = viewModel::setPolicy,
                onPriority = viewModel::setPriority,
            )
            ShareCard(
                state = relay,
                onToggle = { on -> if (on) start(Mode.Share) else viewModel.stop(Mode.Share) },
            )
            MeshCard(mesh, coreVersion, onLeave = { confirmLeave = true })
        }
    }

    if (confirmLeave) {
        AlertDialog(
            onDismissRequest = { confirmLeave = false },
            title = { Text(stringResource(R.string.leave_title)) },
            text = { Text(stringResource(R.string.leave_text)) },
            confirmButton = {
                TextButton(onClick = {
                    confirmLeave = false
                    viewModel.leave()
                }) {
                    Text(stringResource(R.string.leave_confirm), color = MaterialTheme.colorScheme.error)
                }
            },
            dismissButton = {
                TextButton(onClick = { confirmLeave = false }) { Text(stringResource(R.string.cancel)) }
            },
        )
    }
}
