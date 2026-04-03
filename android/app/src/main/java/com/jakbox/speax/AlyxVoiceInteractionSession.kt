package com.jakbox.speax

import android.content.Context
import android.os.Bundle
import android.service.voice.VoiceInteractionSession
import android.util.Log
import android.view.View
import androidx.compose.ui.platform.ComposeView
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color as ComposeColor
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.material3.Text
import androidx.compose.material3.MaterialTheme
import kotlinx.coroutines.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.foundation.border
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Close
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.FloatingActionButtonDefaults
import androidx.compose.ui.platform.LocalConfiguration
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.LifecycleRegistry
import androidx.lifecycle.ViewModelStore
import androidx.lifecycle.ViewModelStoreOwner
import androidx.lifecycle.setViewTreeLifecycleOwner
import androidx.lifecycle.setViewTreeViewModelStoreOwner
import androidx.savedstate.SavedStateRegistry
import androidx.savedstate.SavedStateRegistryController
import androidx.savedstate.SavedStateRegistryOwner
import androidx.savedstate.setViewTreeSavedStateRegistryOwner

/**
 * Handles the user interaction when the assistant is triggered.
 * Implements Lifecycle and State owners to allow Jetpack Compose to run in this context.
 */
class AlyxVoiceInteractionSession(context: Context) : VoiceInteractionSession(context),
    LifecycleOwner, ViewModelStoreOwner, SavedStateRegistryOwner {

    private val lifecycleRegistry = LifecycleRegistry(this)
    private val store = ViewModelStore()
    private val savedStateRegistryController = SavedStateRegistryController.create(this)

    override val lifecycle: Lifecycle get() = lifecycleRegistry
    override val viewModelStore: ViewModelStore get() = store
    override val savedStateRegistry: SavedStateRegistry get() = savedStateRegistryController.savedStateRegistry

    override fun onCreate() {
        super.onCreate()
        Log.d("AlyxSession", "onCreate")
        savedStateRegistryController.performRestore(null)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_CREATE)
        SpeaxManager.init(context)
    }

    override fun onShow(args: Bundle?, showFlags: Int) {
        super.onShow(args, showFlags)
        Log.d("AlyxSession", "onShow flags=$showFlags")
        SpeaxManager.isAssistantMode = true
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_START)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_RESUME)
        
        if (!SpeaxManager.isConnected) {
            SpeaxManager.connect()
        } else {
            CoroutineScope(Dispatchers.IO).launch {
                delay(300)
                SpeaxManager.syncRecordingState()
            }
        }
    }

    override fun onHide() {
        super.onHide()
        Log.d("AlyxSession", "onHide")
        SpeaxManager.isAssistantMode = false
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_PAUSE)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_STOP)
        SpeaxManager.syncRecordingState()
    }


    override fun onDestroy() {
        super.onDestroy()
        Log.d("AlyxSession", "onDestroy")
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_DESTROY)
        store.clear()
    }

    override fun onCreateContentView(): View {
        return ComposeView(context).apply {
            // Attach owners so Compose can work
            setViewTreeLifecycleOwner(this@AlyxVoiceInteractionSession)
            setViewTreeViewModelStoreOwner(this@AlyxVoiceInteractionSession)
            setViewTreeSavedStateRegistryOwner(this@AlyxVoiceInteractionSession)

            setContent {
                SpeaxTheme {
                    AssistantOverlay()
                }
            }
        }
    }

    @Composable
    fun AssistantOverlay() {
        val aiAnimatedBands = remember { mutableStateListOf(0f, 0f, 0f, 0f, 0f) }
        val aiOverallIntensity = remember { mutableFloatStateOf(0f) }
        val aiRotations = remember { mutableStateListOf(0f, 0f, 0f, 0f, 0f) }

        LaunchedEffect(Unit) {
            while (true) {
                androidx.compose.runtime.withFrameNanos {
                    val isActive = SpeaxManager.isConnected && !SpeaxManager.isAiPaused && (SpeaxManager.isGeneratingAi || SpeaxManager.playbackProgress > 0f)
                    val targets = if (isActive) SpeaxManager.aiBands else listOf(0f, 0f, 0f, 0f, 0f)
                    val rmsTarget = if (isActive) (SpeaxManager.aiRms / 6000f).coerceIn(0f, 1f) else 0f
                    
                    // Animate each band individually for the deformation
                    for (i in 0 until 5) {
                        val target = targets.getOrElse(i) { 0f }
                        val current = aiAnimatedBands[i]
                        val step = if (target > current) 0.25f else 0.1f
                        aiAnimatedBands[i] = current + (target - current) * step

                        // Per-layer rotation speed linked to band intensity
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
                    val target = if (SpeaxManager.isConnected && !SpeaxManager.isMicMuted) (SpeaxManager.currentRms / 4000f).coerceIn(0f, 1f) else 0f
                    val step = if (target > micAnimatedIntensity.floatValue) 0.4f else 0.05f
                    micAnimatedIntensity.floatValue += (target - micAnimatedIntensity.floatValue) * step
                }
            }
        }

        // Auto-close logic: 15s of total silence after Assistant finishes
        val notificationSounds = remember { NotificationSounds() }
        LaunchedEffect(SpeaxManager.isGeneratingAi, SpeaxManager.playbackProgress, SpeaxManager.currentRms > 500f) {
            val isAiActive = SpeaxManager.isGeneratingAi || SpeaxManager.playbackProgress > 0f
            val isUserActive = SpeaxManager.currentRms > 500f
            
            if (!isAiActive && !isUserActive) {
                delay(15000)
                notificationSounds.playDisconnect()
                delay(600) // Wait for tone to play
                finish()
            }
        }

        val statusText = SpeaxManager.statusText
        val configuration = LocalConfiguration.current
        val shiftAmount = configuration.screenHeightDp.dp * 0.15f
        
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(ComposeColor.Black.copy(alpha = 0.75f)),
            contentAlignment = Alignment.Center
        ) {
            Box(contentAlignment = Alignment.Center) {
                // AI Pulsing Glow
                AiPulsingGlow(
                    bands = aiAnimatedBands,
                    rotations = aiRotations,
                    modifier = Modifier.size(360.dp).offset(y = -shiftAmount)
                )
                
                // Main Logo Disc
                Box(
                    modifier = Modifier
                        .size(160.dp)
                        .offset(y = -shiftAmount)
                        .background(
                            if (SpeaxManager.isConnected) MaterialTheme.colorScheme.primary 
                            else ComposeColor.White.copy(alpha = 0.08f), 
                            CircleShape
                        )
                        .border(
                            if (SpeaxManager.isConnected) 0.dp else 1.dp, 
                            ComposeColor.White.copy(alpha = 0.15f), 
                            CircleShape
                        )
                        .graphicsLayer {
                            val scale = 1f + (aiOverallIntensity.floatValue * 0.15f)
                            scaleX = scale
                            scaleY = scale
                        },
                    contentAlignment = Alignment.Center
                ) {
                    AlyxLogo(
                        modifier = Modifier.size(80.dp),
                        tint = if (SpeaxManager.isConnected) ComposeColor.White else ComposeColor.White.copy(alpha = 0.5f)
                    )
                }

                // Mic Button (Overlapping Bottom Right of Main Disc)
                Box(
                    modifier = Modifier.offset(x = 65.dp, y = 65.dp - shiftAmount),
                    contentAlignment = Alignment.Center
                ) {
                    MicPulsingGlow(intensityProvider = { micAnimatedIntensity.floatValue })
                    
                    Box(
                        modifier = Modifier
                            .size(72.dp)
                            .background(
                                if (SpeaxManager.isMicMuted) ComposeColor.DarkGray else ComposeColor.White.copy(alpha = 0.25f),
                                CircleShape
                            )
                            .border(6.dp, ComposeColor.Black.copy(alpha = 0.5f), CircleShape)
                            .border(1.dp, ComposeColor.White.copy(alpha = 0.3f), CircleShape)
                            .graphicsLayer {
                                val scale = 1f + (micAnimatedIntensity.floatValue * 0.15f)
                                scaleX = scale
                                scaleY = scale
                            },
                        contentAlignment = Alignment.Center
                    ) {
                        Icon(
                            imageVector = if (SpeaxManager.isMicMuted) Icons.Filled.MicOff else Icons.Filled.Mic,
                            contentDescription = "Mic",
                            tint = ComposeColor.White.copy(alpha = 0.8f),
                            modifier = Modifier.size(28.dp)
                        )
                    }
                }

                // Status & Thinking Text (Centered below main)
                Column(
                    modifier = Modifier.offset(y = 48.dp),
                    horizontalAlignment = Alignment.CenterHorizontally
                ) {
                    Text(
                        text = statusText,
                        color = ComposeColor.White,
                        fontSize = 24.sp,
                        fontWeight = FontWeight.Light,
                        modifier = Modifier.padding(horizontal = 32.dp)
                    )
                    
                    if (SpeaxManager.isGeneratingAi) {
                        Text(
                            text = "${SpeaxManager.assistantName} is thinking...",
                            color = ComposeColor.Cyan.copy(alpha = 0.6f),
                            fontSize = 16.sp,
                            fontWeight = FontWeight.Medium,
                            modifier = Modifier.padding(top = 8.dp)
                        )
                    }
                }

                // Close Button (Centered much lower)
                FloatingActionButton(
                    onClick = { 
                        SpeaxManager.audioEngine.abortPlayback()
                        finish()
                    },
                    shape = CircleShape,
                    modifier = Modifier.size(72.dp).offset(y = shiftAmount),
                    containerColor = ComposeColor.White.copy(alpha = 0.15f),
                    elevation = FloatingActionButtonDefaults.elevation(0.dp)
                ) {
                    Icon(
                        imageVector = Icons.Filled.Close,
                        contentDescription = "Close Assistant",
                        modifier = Modifier.size(32.dp),
                        tint = ComposeColor.White.copy(alpha = 0.7f)
                    )
                }
            }
        }
    }
}
