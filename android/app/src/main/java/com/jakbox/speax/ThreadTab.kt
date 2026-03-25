package com.jakbox.speax

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.text.font.FontWeight

import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.foundation.clickable

@Composable
fun ThreadTab(mainActivity: MainActivity, listState: LazyListState) {
    var inputText by remember { mutableStateOf("") }

    Column(modifier = Modifier.fillMaxSize()) {
        LazyColumn(
            state = listState,
            modifier = Modifier.weight(1f).padding(horizontal = 16.dp),
            contentPadding = PaddingValues(vertical = 16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp)
        ) {
            itemsIndexed(mainActivity.messages) { index, msg ->
                // Calculate if this message is among the most recent 5 of its role
                val sameRoleMessages = mainActivity.messages.filterIndexed { i, m -> m.role == msg.role && i >= index }
                val isRecent = sameRoleMessages.size <= 5

                MessageBubble(msg, index, isRecent, mainActivity)
            }
        }

        // Text Input Row
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .background(MaterialTheme.colorScheme.surface)
                .padding(8.dp),
            verticalAlignment = Alignment.CenterVertically
        ) {
            OutlinedTextField(
                value = inputText,
                onValueChange = { inputText = it },
                modifier = Modifier.weight(1f),
                placeholder = { Text("Type a message...") },
                shape = RoundedCornerShape(24.dp)
            )
            Spacer(Modifier.width(8.dp))
            TextButton(
                onClick = {
                    if (inputText.isNotBlank()) {
                        mainActivity.sendTypedPrompt(inputText)
                        inputText = ""
                    }
                },
                modifier = Modifier.size(48.dp).background(MaterialTheme.colorScheme.primary, CircleShape),
                contentPadding = PaddingValues(0.dp)
            ) {
                Text(">", color = MaterialTheme.colorScheme.onPrimary, fontSize = 20.sp, fontWeight = FontWeight.Bold)
            }
        }
    }
}

@Composable
fun MessageBubble(msg: UiMessage, index: Int, isRecent: Boolean, mainActivity: MainActivity) {
    val isAi = msg.role == "assistant"
    val isSystem = msg.role == "system"
    var userExpanded by remember(msg, index) { mutableStateOf(false) }

    val shouldExpand = if (isSystem) userExpanded else (isRecent || userExpanded)

    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.Start,
        verticalAlignment = Alignment.CenterVertically
    ) {
        // The raw text row
        Row(
            modifier = Modifier
                .weight(1f)
                .clickable { userExpanded = !userExpanded }
        ) {
            Text(
                text = if (isAi) "${SpeaxManager.assistantName}: ${msg.content}" else if (!isSystem) "User: ${msg.content}" else "[System]: ${msg.content}",
                color = if (isSystem) MaterialTheme.colorScheme.onSurface.copy(alpha = 0.3f) else if (isAi) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
                fontSize = if (isSystem) 12.sp else 16.sp,
                fontWeight = if (isAi) FontWeight.Bold else FontWeight.Normal,
                fontStyle = if (isSystem) androidx.compose.ui.text.font.FontStyle.Italic else androidx.compose.ui.text.font.FontStyle.Normal,
                maxLines = if (shouldExpand) Int.MAX_VALUE else 1,
                overflow = TextOverflow.Ellipsis
            )
        }

        if (isSystem) {
            TextButton(
                onClick = { userExpanded = !userExpanded },
                contentPadding = PaddingValues(horizontal = 8.dp, vertical = 2.dp),
                modifier = Modifier.height(24.dp)
            ) {
                Text(
                    text = if (userExpanded) "Collapse" else "Expand",
                    fontSize = 10.sp,
                    color = MaterialTheme.colorScheme.primary
                )
            }
        }

        // Minimal Delete Button (Only on User turns, sits to the far right)
        if (!isSystem && !isAi) {
            Spacer(Modifier.width(8.dp))
            TextButton(
                onClick = { mainActivity.deleteMessagePair(index) },
                modifier = Modifier.size(24.dp).background(MaterialTheme.colorScheme.surfaceVariant, CircleShape),
                contentPadding = PaddingValues(0.dp),
                shape = CircleShape
            ) {
                Text("X", color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f), fontSize = 12.sp)
            }
        }
    }
}
