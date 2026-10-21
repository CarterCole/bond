package com.cartertech.bond.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import com.cartertech.bond.R
import com.cartertech.bond.core.MeshInfo

@Composable
fun MeshCard(mesh: MeshInfo, coreVersion: String, onLeave: () -> Unit) {
    ElevatedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(stringResource(R.string.mesh_title), style = MaterialTheme.typography.titleMedium)
            StatRow(stringResource(R.string.mesh_device), mesh.name)
            StatRow(stringResource(R.string.mesh_address), "${mesh.address}/${mesh.prefix}")
            StatRow(stringResource(R.string.mesh_aggregator), mesh.aggregator)
            StatRow(stringResource(R.string.mesh_id), mesh.meshId)
            if (coreVersion.isNotEmpty()) StatRow(stringResource(R.string.mesh_core), coreVersion)
            OutlinedButton(
                onClick = onLeave,
                colors = ButtonDefaults.outlinedButtonColors(contentColor = MaterialTheme.colorScheme.error),
            ) {
                Text(stringResource(R.string.leave_action))
            }
        }
    }
}
