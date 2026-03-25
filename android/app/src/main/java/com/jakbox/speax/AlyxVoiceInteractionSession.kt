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
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_START)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_RESUME)
        
        if (!SpeaxManager.isConnected) {
            SpeaxManager.connect()
        } else if (!SpeaxManager.isMicMuted) {
            CoroutineScope(Dispatchers.IO).launch {
                delay(300)
                if (!SpeaxManager.isMicMuted) {
                    SpeaxManager.audioEngine.startRecording()
                }
            }
        }
    }

    override fun onHide() {
        super.onHide()
        Log.d("AlyxSession", "onHide")
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_PAUSE)
        lifecycleRegistry.handleLifecycleEvent(Lifecycle.Event.ON_STOP)
        SpeaxManager.audioEngine.stopRecording()
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
        val aiAnimatedIntensity = remember { mutableFloatStateOf(0f) }
        LaunchedEffect(Unit) {
            while (true) {
                androidx.compose.runtime.withFrameNanos {
                    val target = if (SpeaxManager.isConnected && !SpeaxManager.isAiPaused) (SpeaxManager.aiRms / 6000f).coerceIn(0f, 1f) else 0f
                    val step = if (target > aiAnimatedIntensity.floatValue) 0.4f else 0.05f
                    aiAnimatedIntensity.floatValue += (target - aiAnimatedIntensity.floatValue) * step
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
        
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(ComposeColor.Black.copy(alpha = 0.75f)),
            contentAlignment = Alignment.Center
        ) {
            Column(
                horizontalAlignment = Alignment.CenterHorizontally,
                verticalArrangement = Arrangement.Center,
                modifier = Modifier.fillMaxSize()
            ) {
                // Central Pulse Area
                Box(contentAlignment = Alignment.Center) {
                    // AI Pulsing Glow
                    AiPulsingGlow(
                        intensityProvider = { aiAnimatedIntensity.floatValue },
                        modifier = Modifier.size(240.dp)
                    )
                    
                    // Main Logo Disc with Scale Transform
                    // Match the size and color of the main app button (160dp, primary color)
                    Box(
                        modifier = Modifier
                            .size(160.dp)
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
                                val scale = 1f + (aiAnimatedIntensity.floatValue * 0.15f)
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
                }

                Spacer(modifier = Modifier.height(48.dp))

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

                Spacer(modifier = Modifier.height(64.dp))

                // User Mic Disc Pulse at the bottom
                Box(contentAlignment = Alignment.Center) {
                    MicPulsingGlow(intensityProvider = { micAnimatedIntensity.floatValue })
                    
                    Box(
                        modifier = Modifier
                            .size(64.dp)
                            .background(
                                if (SpeaxManager.isMicMuted) ComposeColor.DarkGray else ComposeColor.White.copy(alpha = 0.25f),
                                CircleShape
                            )
                            .border(1.dp, ComposeColor.White.copy(alpha = 0.3f), CircleShape)
                            .graphicsLayer {
                                val scale = 1f + (micAnimatedIntensity.floatValue * 0.15f)
                                scaleX = scale
                                scaleY = scale
                            },
                        contentAlignment = Alignment.Center
                    ) {
                        // We can add the icon here for better clarity as a "mic disc"
                        androidx.compose.material3.Icon(
                            imageVector = if (SpeaxManager.isMicMuted) androidx.compose.material.icons.Icons.Filled.MicOff else androidx.compose.material.icons.Icons.Filled.Mic,
                            contentDescription = "Mic",
                            tint = ComposeColor.White.copy(alpha = 0.8f),
                            modifier = Modifier.size(24.dp)
                        )
                    }
                }
            }
        }
    }
}
