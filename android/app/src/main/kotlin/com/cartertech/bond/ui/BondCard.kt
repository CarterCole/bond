package com.cartertech.bond.ui

import android.text.format.Formatter
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.key
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.cartertech.bond.R
import com.cartertech.bond.core.PathStatus
import com.cartertech.bond.core.Policy
import com.cartertech.bond.vpn.VpnState
import kotlin.math.roundToInt

@Composable
fun BondCard(
    state: VpnState,
    onToggle: (Boolean) -> Unit,
    onPolicy: (String, Policy) -> Unit,
    onPriority: (Boolean) -> Unit,
) {
    val status = when (state) {
        is VpnState.Off -> state.error
        VpnState.Starting -> stringResource(R.string.vpn_connecting)
        is VpnState.Running ->
            if (state.status.connected) {
                stringResource(R.string.vpn_connected, state.status.address, formatDuration(state.status.uptimeSec))
            } else {
                stringResource(R.string.vpn_connecting)
            }
    }
    ModeCard(
        title = stringResource(R.string.mode_bond),
        description = stringResource(R.string.mode_bond_description),
        checked = state !is VpnState.Off,
        onCheckedChange = onToggle,
        status = status,
        statusIsError = state is VpnState.Off,
    ) {
        if (state !is VpnState.Running) return@ModeCard
        if (state.status.paths.isEmpty()) {
            Text(stringResource(R.string.vpn_no_networks), style = MaterialTheme.typography.bodyMedium)
        }
        for (path in state.status.paths) {
            key(path.name) { PathCard(path, onPolicy = { onPolicy(path.name, it) }) }
        }
        HorizontalDivider()
        SwitchRow(
            title = stringResource(R.string.priority_title),
            description = stringResource(R.string.priority_description),
            checked = state.status.priorityAll,
            onCheckedChange = onPriority,
        )
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun PathCard(path: PathStatus, onPolicy: (Policy) -> Unit) {
    val context = LocalContext.current
    val off = path.policy == Policy.Off.wire
    Surface(
        shape = MaterialTheme.shapes.medium,
        color = MaterialTheme.colorScheme.surfaceContainerHighest,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                val dot = when {
                    off -> MaterialTheme.colorScheme.outline
                    path.up -> LinkUp
                    else -> MaterialTheme.colorScheme.error
                }
                Box(Modifier.size(10.dp).background(dot, CircleShape))
                Text(pathTitle(path.name), style = MaterialTheme.typography.titleSmall, modifier = Modifier.weight(1f))
                Text(
                    when {
                        off -> stringResource(R.string.path_off)
                        path.up -> stringResource(R.string.path_quality, path.rttMs.roundToInt(), formatLoss(path.loss))
                        else -> stringResource(R.string.path_down)
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
                val small = MaterialTheme.typography.bodySmall
                Text(stringResource(R.string.path_sending, formatRate(path.txBps)), style = small)
                Text(stringResource(R.string.path_receiving, formatRate(path.rxBps)), style = small)
                Text(
                    Formatter.formatShortFileSize(context, path.txBytes + path.rxBytes),
                    style = small,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                    textAlign = TextAlign.End,
                )
            }
            SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                Policy.entries.forEachIndexed { index, policy ->
                    SegmentedButton(
                        selected = path.policy == policy.wire,
                        onClick = { onPolicy(policy) },
                        shape = SegmentedButtonDefaults.itemShape(index, Policy.entries.size),
                        // Four choices have to fit a narrow phone: no check mark, tight padding.
                        icon = {},
                        contentPadding = PaddingValues(horizontal = 4.dp),
                    ) {
                        Text(policyLabel(policy), style = MaterialTheme.typography.labelMedium, maxLines = 1)
                    }
                }
            }
        }
    }
}

/** The core's path names, in words: "wifi", "ws:wifi" (the WebSocket fallback), "relay:<name>". */
@Composable
private fun pathTitle(name: String): String = when {
    name == "wifi" -> stringResource(R.string.path_wifi)
    name == "cellular" -> stringResource(R.string.path_cellular)
    name.startsWith("ws:") -> stringResource(R.string.path_fallback, pathTitle(name.removePrefix("ws:")))
    name.startsWith("relay:") -> stringResource(R.string.path_relay, name.removePrefix("relay:"))
    else -> name
}

@Composable
private fun policyLabel(policy: Policy): String = stringResource(
    when (policy) {
        Policy.Primary -> R.string.policy_primary
        Policy.Bonded -> R.string.policy_bonded
        Policy.Backup -> R.string.policy_backup
        Policy.Off -> R.string.policy_off
    },
)
