package com.cartertech.bond.ui

import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.cartertech.bond.R
import com.cartertech.bond.core.LocalNetwork

@Composable
fun JoinScreen(viewModel: BondViewModel) {
    val context = LocalContext.current
    val state by viewModel.join.collectAsStateWithLifecycle()
    var name by rememberSaveable { mutableStateOf(Build.MODEL.orEmpty()) }
    var code by rememberSaveable { mutableStateOf("") }
    var scanError by rememberSaveable { mutableStateOf<String?>(null) }

    // Whatever the answer, try: if it was no, the core's own error says what could not be reached.
    val localNetwork = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        viewModel.join(code, name)
    }

    fun join() {
        scanError = null
        // An aggregator on this LAN (a test setup, typically) is out of reach on Android 17 until
        // the local network permission is granted.
        val host = LocalNetwork.hostOfJoinCode(code)
        if (host != null && LocalNetwork.isLocal(host) && LocalNetwork.permissionMissing(context)) {
            localNetwork.launch(LocalNetwork.PERMISSION)
        } else {
            viewModel.join(code, name)
        }
    }

    Scaffold { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .imePadding()
                .verticalScroll(rememberScrollState())
                .padding(24.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            Text(stringResource(R.string.app_name), style = MaterialTheme.typography.displaySmall)
            Text(
                stringResource(R.string.join_intro),
                style = MaterialTheme.typography.bodyLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedTextField(
                value = name,
                onValueChange = { name = it },
                label = { Text(stringResource(R.string.join_device_name)) },
                singleLine = true,
                enabled = !state.busy,
                keyboardOptions = KeyboardOptions(
                    capitalization = KeyboardCapitalization.Words,
                    imeAction = ImeAction.Next,
                ),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = code,
                onValueChange = { code = it },
                label = { Text(stringResource(R.string.join_code)) },
                placeholder = { Text(stringResource(R.string.join_code_placeholder)) },
                supportingText = { Text(stringResource(R.string.join_code_help)) },
                minLines = 2,
                enabled = !state.busy,
                keyboardOptions = KeyboardOptions(
                    capitalization = KeyboardCapitalization.None,
                    autoCorrectEnabled = false,
                    keyboardType = KeyboardType.Uri,
                    imeAction = ImeAction.Done,
                ),
                modifier = Modifier.fillMaxWidth(),
            )
            Row(
                horizontalArrangement = Arrangement.spacedBy(12.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                OutlinedButton(
                    onClick = {
                        scanError = null
                        viewModel.clearJoinError()
                        scanQrCode(context, onCode = { code = it.trim() }, onError = { scanError = it })
                    },
                    enabled = !state.busy,
                ) {
                    Text(stringResource(R.string.join_scan))
                }
                Button(
                    onClick = ::join,
                    enabled = !state.busy && code.isNotBlank() && name.isNotBlank(),
                ) {
                    Text(stringResource(R.string.join_action))
                }
                if (state.busy) CircularProgressIndicator(Modifier.size(24.dp), strokeWidth = 2.dp)
            }
            (state.error ?: scanError)?.let {
                Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
            }
        }
    }
}
