package com.jakbox.speax

import androidx.compose.animation.core.Spring
import androidx.compose.animation.core.spring
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.animation.core.LinearEasing
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.drawscope.translate
import androidx.compose.ui.graphics.drawscope.rotate
import androidx.compose.animation.core.*
import androidx.compose.ui.geometry.Offset
import kotlin.math.*
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
import androidx.compose.ui.unit.sp
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.platform.LocalConfiguration

@Composable
fun HomeTab(mainActivity: MainActivity) {
    val aiAnimatedBands = remember { mutableStateListOf(0f, 0f, 0f, 0f, 0f) }
    val aiOverallIntensity = remember { mutableFloatStateOf(0f) }
    val aiRotations = remember { mutableStateListOf(0f, 0f, 0f, 0f, 0f) }
    
    LaunchedEffect(Unit) {
        while (true) {
            androidx.compose.runtime.withFrameNanos {
                val isActive = mainActivity.isConnected && !mainActivity.isAiPaused && (mainActivity.isGeneratingAi || mainActivity.playbackProgress > 0f)
                val targets = if (isActive) mainActivity.aiBands else listOf(0f, 0f, 0f, 0f, 0f)
                val rmsTarget = if (isActive) (mainActivity.aiRms / 6000f).coerceIn(0f, 1f) else 0f
                
                // Animate each band individually for the deformation
                for (i in 0 until 5) {
                    val target = targets.getOrElse(i) { 0f }
                    val current = aiAnimatedBands[i]
                    val step = if (target > current) 0.25f else 0.1f
                    aiAnimatedBands[i] = current + (target - current) * step

                    // Per-layer rotation speed linked to band intensity
                    // Much lower base rate (0.02f) as requested
                    val baseSpeed = 0.02f 
                    val intensityBonus = aiAnimatedBands[i] * 0.8f
                    aiRotations[i] = (aiRotations[i] + baseSpeed + intensityBonus)
                }
                
                // Animate overall intensity for scaling the main button
                val step = if (rmsTarget > aiOverallIntensity.floatValue) 0.4f else 0.05f
                aiOverallIntensity.floatValue += (rmsTarget - aiOverallIntensity.floatValue) * step
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
            AiPulsingGlow(aiAnimatedBands, aiRotations, modifier = Modifier.offset(y = -shiftAmount))

            // Main Button
            FloatingActionButton(
                onClick = { mainActivity.toggleConnection() },
                shape = CircleShape,
                modifier = Modifier
                    .size(160.dp)
                    .offset(y = -shiftAmount)
                    .graphicsLayer {
                        val scale = 1f + (aiOverallIntensity.floatValue * 0.15f)
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

            if (!mainActivity.isConnected) {
                Text(
                    text = "Tap to Connect",
                    color = MaterialTheme.colorScheme.secondary.copy(alpha = 0.7f),
                    fontSize = 18.sp,
                    fontWeight = FontWeight.Light,
                    modifier = Modifier.offset(y = -shiftAmount + 110.dp)
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
fun AiPulsingGlow(bands: List<Float>, rotations: List<Float>, modifier: Modifier = Modifier) {
    val theme = SpeaxManager.currentTheme
    val baseColor = theme?.secondary?.let { Color(android.graphics.Color.parseColor(it)) } ?: MaterialTheme.colorScheme.secondary
    
    // Reducing to 5 layers as requested
    val layers = listOf(
        Triple(80.dp, 0.25f, 60f), // Bass - High scaling
        Triple(80.dp, 0.4f, 50f),
        Triple(80.dp, 0.55f, 40f),
        Triple(80.dp, 0.7f, 30f),
        Triple(80.dp, 0.85f, 20f)  // Highs - Low scaling
    )

    Box(modifier = modifier.size(400.dp), contentAlignment = Alignment.Center) {
        Canvas(modifier = Modifier.fillMaxSize()) {
            val canvasCenter = Offset(size.width / 2f, size.height / 2f)
            
            layers.forEachIndexed { index, config ->
                val (baseRadiusDp, alpha, deformMult) = config
                val baseRadius = baseRadiusDp.toPx()
                
                val rotation = rotations.getOrElse(index) { 0f }
                val intensity = bands.getOrElse(index) { 0f }
                
                // Subtle Orbital Drift Logic (max 8dp)
                val driftAngle = Math.toRadians(rotation.toDouble() * 0.5).toFloat()
                val driftDistance = (intensity * 8.dp.toPx())
                val centerOffset = Offset(
                    cos(driftAngle) * driftDistance,
                    sin(driftAngle) * driftDistance
                )
                
                val currentCenter = canvasCenter + centerOffset
                val currentRadius = baseRadius + (intensity * deformMult.dp.toPx())
                
                // DYNAMIC Star-Burst Gradient: Scales with the intensity
                // Original stop logic (0.0 -> 0.8) for solid core
                val radialBrush = Brush.radialGradient(
                    0.0f to baseColor.copy(alpha = alpha),
                    0.8f to baseColor.copy(alpha = alpha * 0.8f),
                    1.0f to Color.Transparent,
                    center = currentCenter,
                    radius = currentRadius.coerceAtLeast(1f)
                )

                // Back to drawCircle without deformation as requested
                drawCircle(
                    brush = radialBrush,
                    center = currentCenter,
                    radius = currentRadius
                )
                
                // Edge stroke for crisp separation
                drawCircle(
                    color = baseColor.copy(alpha = alpha * 0.3f),
                    center = currentCenter,
                    radius = currentRadius,
                    style = Stroke(width = 1.dp.toPx())
                )
            }
        }
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
