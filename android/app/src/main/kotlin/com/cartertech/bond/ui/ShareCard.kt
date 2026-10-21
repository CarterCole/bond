package com.cartertech.bond.ui

import android.text.format.Formatter
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import com.cartertech.bond.R
import com.cartertech.bond.relay.Announcement
import com.cartertech.bond.relay.RelayState

@Composable
fun ShareCard(state: RelayState, onToggle: (Boolean) -> Unit) {
    val context = LocalContext.current
    val status = when (state) {
        is RelayState.Off -> state.error
        RelayState.Starting -> stringResource(R.string.share_starting)
        is RelayState.Waiting -> state.error ?: stringResource(R.string.share_waiting)
        is RelayState.Running ->
            pluralStringResource(R.plurals.share_hubs, state.status.clients, state.status.clients)
    }
    ModeCard(
        title = stringResource(R.string.mode_share),
        description = stringResource(R.string.mode_share_description),
        checked = state !is RelayState.Off,
        onCheckedChange = onToggle,
        status = status,
        statusIsError = state is RelayState.Off || (state is RelayState.Waiting && state.error != null),
    ) {
        if (state !is RelayState.Running) return@ModeCard
        val relay = state.status
        StatRow(
            stringResource(R.string.share_sent),
            Formatter.formatShortFileSize(context, relay.upBytes),
        )
        StatRow(
            stringResource(R.string.share_returned),
            Formatter.formatShortFileSize(context, relay.downBytes),
        )
        if (relay.refused > 0) StatRow(stringResource(R.string.share_refused), relay.refused.toString())
        StatRow(stringResource(R.string.share_upstream), relay.upstream)
        Text(
            when (state.announcement) {
                Announcement.Pending -> stringResource(R.string.share_announcing)
                Announcement.Announced -> stringResource(R.string.share_announced)
                Announcement.Failed -> stringResource(R.string.share_not_announced)
            } + (state.lanAddress?.let { "\n" + stringResource(R.string.share_address, it) } ?: ""),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}
