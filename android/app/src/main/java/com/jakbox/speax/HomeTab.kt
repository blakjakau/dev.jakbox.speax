package com.jakbox.speax

import androidx.compose.animation.core.Spring
import androidx.compose.animation.core.spring
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.animation.core.LinearEasing
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.border
import androidx.compose.foundation.background
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.unit.dp
import androidx.compose.ui.platform.LocalConfiguration

@Composable
fun HomeTab(mainActivity: MainActivity) {
    val aiAnimatedIntensity = remember { mutableFloatStateOf(0f) }
    LaunchedEffect(Unit) {
        while (true) {
            androidx.compose.runtime.withFrameNanos {
                val target = if (mainActivity.isConnected && !mainActivity.isAiPaused) (mainActivity.aiRms / 6000f).coerceIn(0f, 1f) else 0f
                val step = if (target > aiAnimatedIntensity.floatValue) 0.4f else 0.05f
                aiAnimatedIntensity.floatValue += (target - aiAnimatedIntensity.floatValue) * step
            }
        }
    }

    val micAnimatedIntensity = remember { mutableFloatStateOf(0f) }
    LaunchedEffect(Unit) {
        while (true) {
            androidx.compose.runtime.withFrameNanos {
                val target = if (mainActivity.isConnected && !mainActivity.isMicMuted) (mainActivity.currentRms / 4000f).coerceIn(0f, 1f) else 0f
                val step = if (target > micAnimatedIntensity.floatValue) 0.4f else 0.05f
                micAnimatedIntensity.floatValue += (target - micAnimatedIntensity.floatValue) * step
            }
        }
    }

    Box(modifier = Modifier.fillMaxSize()) {
        val shiftAmount = LocalConfiguration.current.screenHeightDp.dp * 0.15f

        Box(
            modifier = Modifier.fillMaxSize(),
            contentAlignment = Alignment.Center
        ) {
            // AI Pulsing Glow
            AiPulsingGlow({ aiAnimatedIntensity.floatValue }, modifier = Modifier.offset(y = -shiftAmount))

            // Main Button
            FloatingActionButton(
                onClick = { mainActivity.toggleConnection() },
                shape = CircleShape,
                modifier = Modifier
                    .size(160.dp)
                    .offset(y = -shiftAmount)
                    .graphicsLayer {
                        val scale = 1f + (aiAnimatedIntensity.floatValue * 0.15f)
                        scaleX = scale
                        scaleY = scale
                    },
                containerColor = if (mainActivity.isConnected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.surface,
                elevation = FloatingActionButtonDefaults.elevation(
                    defaultElevation = if (mainActivity.isConnected) 12.dp else 4.dp
                )
            ) {
                AlyxLogo(
                    modifier = Modifier.size(64.dp),
                    tint = if (mainActivity.isConnected) Color.White else MaterialTheme.colorScheme.primary
                )
            }

            if (mainActivity.isConnected) {
                // Play/Pause (Centered Below)
                FloatingActionButton(
                    onClick = { mainActivity.toggleAiPause() },
                    shape = CircleShape,
                    modifier = Modifier.size(72.dp).offset(y = shiftAmount),
                    containerColor = if (mainActivity.isAiPaused) MaterialTheme.colorScheme.background else MaterialTheme.colorScheme.surfaceVariant
                ) {
                    Icon(
                        imageVector = if (mainActivity.isAiPaused) Icons.Filled.PlayArrow else Icons.Filled.Pause,
                        contentDescription = "Play/Pause",
                        modifier = Modifier.size(32.dp),
                        tint = if (mainActivity.isAiPaused) MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f) else MaterialTheme.colorScheme.onSurface
                    )
                }

                // Mute Mic (Overlapping Bottom Right of Main Button)
                Box(modifier = Modifier.offset(x = 65.dp, y = 65.dp - shiftAmount), contentAlignment = Alignment.Center) {
                    // Mic Pulsing Glow
                    MicPulsingGlow({ micAnimatedIntensity.floatValue })
                    FloatingActionButton(
                        onClick = { mainActivity.toggleMicMute() },
                        shape = CircleShape,
                        modifier = Modifier
                            .size(84.dp) // 72dp body + 6dp border on each side
                            .border(6.dp, MaterialTheme.colorScheme.surface, CircleShape)
                            .graphicsLayer {
                                val scale = 1f + (micAnimatedIntensity.floatValue * 0.15f)
                                scaleX = scale
                                scaleY = scale
                            },
                        containerColor = if (mainActivity.isMicMuted) MaterialTheme.colorScheme.background else MaterialTheme.colorScheme.surfaceVariant
                    ) {
                        Icon(
                            imageVector = if (mainActivity.isMicMuted) Icons.Filled.MicOff else Icons.Filled.Mic,
                            contentDescription = "Mute Mic",
                            modifier = Modifier.size(32.dp),
                            tint = if (mainActivity.isMicMuted) MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f) else MaterialTheme.colorScheme.onSurface
                        )
                    }
                }
            }
        }
    }
}

@Composable
fun AiPulsingGlow(intensityProvider: () -> Float, modifier: Modifier = Modifier) {
    val baseColor = MaterialTheme.colorScheme.secondary

    Canvas(
        modifier = modifier
            .size(200.dp) // Tighter outer bounds
            .graphicsLayer {
                val scale = 1f + (intensityProvider() * 0.15f)
                scaleX = scale
                scaleY = scale
            }
    ) {
        val intensity = intensityProvider()
        val glowColor = baseColor.copy(alpha = (0.8f * intensity).coerceIn(0f, 1f))
        drawCircle(
            brush = Brush.radialGradient(
                0.0f to glowColor,
                0.75f to glowColor, // Stay solid underneath the 160dp button (80 / 100 = 0.8)
                1.0f to Color.Transparent,
                radius = size.width / 2f
            )
        )
    }
}

@Composable
fun MicPulsingGlow(intensityProvider: () -> Float) {
    val baseColor = MaterialTheme.colorScheme.secondary

    Canvas(
        modifier = Modifier
            .size(116.dp) // Tighter outer bounds
            .graphicsLayer {
                val scale = 1f + (intensityProvider() * 0.15f)
                scaleX = scale
                scaleY = scale
            }
    ) {
        val intensity = intensityProvider()
        val glowColor = baseColor.copy(alpha = (0.8f * intensity).coerceIn(0f, 1f))
        drawCircle(
            brush = Brush.radialGradient(
                0.0f to glowColor,
                0.8f to glowColor, // Stay solid underneath the 84dp socket (42 / 58 = 0.72)
                1f to Color.Transparent,
                radius = size.width / 2f
            )
        )
    }
}
