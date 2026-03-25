package com.jakbox.speax

import android.Manifest
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.util.Log
import android.os.PowerManager
import android.webkit.CookieManager
import android.media.AudioManager
import android.media.AudioDeviceInfo
import android.media.MediaRecorder
import android.os.Build
import android.media.session.MediaSession
import android.media.session.PlaybackState
import android.view.KeyEvent
import java.io.File
import android.speech.RecognitionListener
import android.speech.RecognizerIntent
import android.speech.SpeechRecognizer
import androidx.browser.customtabs.CustomTabsIntent
import androidx.activity.ComponentActivity
import androidx.lifecycle.lifecycleScope
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.view.WindowCompat
import androidx.core.content.ContextCompat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.LinearEasing
import androidx.compose.foundation.background
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.border
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Memory
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.PowerOff
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.automirrored.filled.List
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.delay
import kotlinx.coroutines.Job
import kotlinx.coroutines.withContext
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import android.content.BroadcastReceiver
import android.content.IntentFilter

// UiMessage and ThreadItem removed (now in SpeaxManager)

class MainActivity : ComponentActivity() {

    val speaxWebSocket get() = SpeaxManager.speaxWebSocket
    val audioEngine get() = SpeaxManager.audioEngine
    private val notificationSounds = NotificationSounds()
    private val httpClient = OkHttpClient()
    private var mediaSession: MediaSession? = null

    // State variables that Compose will observe to update the UI instantly
    val messages = SpeaxManager.messages
    var isConnected by SpeaxManager::isConnected
    var statusText by SpeaxManager::statusText
    var isGeneratingAi by SpeaxManager::isGeneratingAi
    var currentRms by SpeaxManager::currentRms
    var aiRms by SpeaxManager::aiRms
    var playbackProgress by SpeaxManager::playbackProgress
    var isMicMuted by SpeaxManager::isMicMuted
    var isAiPaused by SpeaxManager::isAiPaused
    private var wakeLock: PowerManager.WakeLock? = null
    private val focusChangeListener = AudioManager.OnAudioFocusChangeListener { _ -> }
    private var audioFocusRequest: Any? = null

    // Settings State
    var aiProvider by SpeaxManager::aiProvider
    var geminiApiKey by SpeaxManager::geminiApiKey
    var aiModel by SpeaxManager::aiModel
    var aiVoice by SpeaxManager::aiVoice
    var availableModels by SpeaxManager::availableModels
    var isLoadingModels by SpeaxManager::isLoadingModels
    var availableVoices by SpeaxManager::availableVoices
    var isLoadingVoices by SpeaxManager::isLoadingVoices
    var userName by SpeaxManager::userName
    var userBio by SpeaxManager::userBio
    var googleName by SpeaxManager::googleName
    var useNativeStt by SpeaxManager::useNativeStt
    var isNativeSttSupported by SpeaxManager::isNativeSttSupported
    var useLocalTts by SpeaxManager::useLocalTts
    var passiveAssistant by SpeaxManager::passiveAssistant
    var micProfile by SpeaxManager::micProfile

    // Memory & Thread State
    var memorySummary by SpeaxManager::memorySummary
    var activeThreadId by SpeaxManager::activeThreadId
    var archiveTurns by SpeaxManager::archiveTurns
    var maxArchiveTurns by SpeaxManager::maxArchiveTurns
    var estTokens by SpeaxManager::estTokens
    var maxTokens by SpeaxManager::maxTokens
    val tokenUsage = SpeaxManager.tokenUsage
    var activeThreadName by SpeaxManager::activeThreadName
    val availableThreads = SpeaxManager.availableThreads

    // Auth State
    var sessionCookie by SpeaxManager::sessionCookie
    private var wasSuccessfullyConnected = false

    // Native STT
    private var speechRecognizer: SpeechRecognizer? = null
    private var nativeSttIntent: Intent? = null

    private var isAppInForeground = false
    private var cpuWakeLock: PowerManager.WakeLock? = null

	
	private val serverUrl = "wss://speax.jakbox.dev/ws"
    private val authUrl = "https://speax.jakbox.dev/auth/login?client=android"

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Enable edge-to-edge so Compose can detect and measure the keyboard (IME)
        WindowCompat.setDecorFitsSystemWindows(window, false)
        
        // Tell legacy Android to STOP resizing the window, letting Compose's .imePadding() do 100% of the work
        window.setSoftInputMode(android.view.WindowManager.LayoutParams.SOFT_INPUT_ADJUST_NOTHING)

        // Initialize SpeaxManager
        SpeaxManager.init(this)

        // 2. Request Mic & Notification Permissions
        val requestPermissionLauncher = registerForActivityResult(
            ActivityResultContracts.RequestMultiplePermissions()
        ) { permissions ->
            if (permissions[Manifest.permission.RECORD_AUDIO] == true) {
                if (sessionCookie != null && !isConnected) {
                    connectWebSocket()
                }
            } else {
                statusText = "Mic permission denied!"
            }
        }
        
        val perms = mutableListOf(Manifest.permission.RECORD_AUDIO)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            perms.add(Manifest.permission.POST_NOTIFICATIONS)
        }
        requestPermissionLauncher.launch(perms.toTypedArray())

        // Handle potential deep link from OAuth redirect
        handleIntent(intent)

        setupMediaSession()
        setupSpeechRecognizer()

        // 3. Auto-connect if we have a session
        if (sessionCookie != null) {
            // Only auto-connect if we already have permission.
            // If not, the permission callback above will trigger it after the user grants it.
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == android.content.pm.PackageManager.PERMISSION_GRANTED) {
                connectWebSocket()
            }
        }
		
		// Register the local receiver so we can use mute/playpause hardware
	    val filter = IntentFilter("SPEAX_HARDWARE_BTN")
	    ContextCompat.registerReceiver(this, hardwareButtonReceiver, filter, ContextCompat.RECEIVER_NOT_EXPORTED)
	        // 3. Render UI
        setContent {
            SpeaxTheme {
                if (sessionCookie == null) {
                    LoginScreen(authUrl)
                } else {
                    ChatScreen()
                }
            }
        }
    }

    override fun onResume() {
        super.onResume()
        isAppInForeground = true
        updateWakeLocks()
    }

    override fun onPause() {
        super.onPause()
        isAppInForeground = false
        updateWakeLocks()
    }

	// binding the media play/pause controls
	private val hardwareButtonReceiver = object : BroadcastReceiver() {
	    override fun onReceive(context: Context?, intent: Intent?) {
	        if (intent?.action == "SPEAX_HARDWARE_BTN") {
	            val keyCode = intent.getIntExtra("keycode", -1)
	            when (keyCode) {
	                KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE,
	                KeyEvent.KEYCODE_HEADSETHOOK,
	                KeyEvent.KEYCODE_MEDIA_PLAY,
	                KeyEvent.KEYCODE_MEDIA_PAUSE -> toggleAiPause()
	                
	                KeyEvent.KEYCODE_MUTE,
	                KeyEvent.KEYCODE_VOLUME_MUTE -> toggleMicMute()
	            }
	        }
	    }
	}

    /**
     * Centralized wake lock management.
     * - FLAG_KEEP_SCREEN_ON: Prevents display sleep when mic is active AND activity is visible.
     * - PARTIAL_WAKE_LOCK: Prevents CPU sleep when mic is active (even with screen off).
     */
    private fun updateWakeLocks() {
        val micActive = isConnected && !isMicMuted

        // Screen wake: only when activity is in the foreground
        if (micActive && isAppInForeground) {
            window.addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        } else {
            window.clearFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        }
    }

    override fun onNewIntent(intent: Intent?) {
        super.onNewIntent(intent)
        handleIntent(intent)
    }

    private fun handleIntent(intent: Intent?) {
        val action = intent?.action
        val data = intent?.data
        
        if (Intent.ACTION_VIEW == action && data != null && data.scheme == "speax") {
            val sessionParam = data.getQueryParameter("session")
            if (sessionParam != null) {
                val exactCookie = "speax_session=$sessionParam"
                getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().putString("session_cookie", exactCookie).apply()
                sessionCookie = exactCookie
                
                val nameParam = data.getQueryParameter("name")
                if (nameParam != null) {
                    googleName = java.net.URLDecoder.decode(nameParam, "UTF-8")
                    getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().putString("google_name", googleName).apply()
                }
            }
        }
    }

    private fun updateBackgroundService() {
	    // Background service is now managed centrally by SpeaxManager.connect() / disconnect()
	    // We just trigger a state update for the notification if needed
	    if (isConnected) {
            val intent = Intent(this, SpeaxService::class.java).apply {
                action = "START"
                mediaSession?.sessionToken?.let { putExtra("session_token", it) }
                putExtra("is_muted", isMicMuted || isAiPaused)
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) startForegroundService(intent) else startService(intent)
	    }
	}

    private fun setupMediaSession() {
        mediaSession = MediaSession(this, "SpeaxMediaSession").apply {
            @Suppress("DEPRECATION")
            setFlags(MediaSession.FLAG_HANDLES_MEDIA_BUTTONS or MediaSession.FLAG_HANDLES_TRANSPORT_CONTROLS)
            
            // Tell the OS we are in a Voice Communication (Call) session!
            // This is required for many headsets to route buttons correctly when the mic is open.
            val callAttributes = android.media.AudioAttributes.Builder()
                .setUsage(android.media.AudioAttributes.USAGE_VOICE_COMMUNICATION)
                .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                .build()
            setPlaybackToLocal(callAttributes)

            setCallback(object : MediaSession.Callback() {
                override fun onPlay() { runOnUiThread { if (isAiPaused) toggleAiPause() } }
                override fun onPause() { runOnUiThread { if (!isAiPaused) toggleAiPause() } }
                override fun onMediaButtonEvent(mediaButtonIntent: Intent): Boolean {
				    val keyEvent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
				        mediaButtonIntent.getParcelableExtra(Intent.EXTRA_KEY_EVENT, KeyEvent::class.java)
				    } else {
				        @Suppress("DEPRECATION")
				        mediaButtonIntent.getParcelableExtra<KeyEvent>(Intent.EXTRA_KEY_EVENT)
				    }
				
				    if (keyEvent != null) {
				        val keyCode = keyEvent.keyCode
				        val action = keyEvent.action
				        
				        if (action == KeyEvent.ACTION_DOWN) {
				            // Acknowledge the press so the OS knows we have focus, but do nothing yet
				            when (keyCode) {
				                KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE,
				                KeyEvent.KEYCODE_HEADSETHOOK,
				                KeyEvent.KEYCODE_MEDIA_PLAY,
				                KeyEvent.KEYCODE_MEDIA_PAUSE,
				                KeyEvent.KEYCODE_MUTE,
				                KeyEvent.KEYCODE_VOLUME_MUTE -> return true
				            }
				        } else if (action == KeyEvent.ACTION_UP) {
				            // Execute the toggle when the user releases the button
				            when (keyCode) {
				                KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE,
				                KeyEvent.KEYCODE_HEADSETHOOK,
				                KeyEvent.KEYCODE_MEDIA_PLAY,
				                KeyEvent.KEYCODE_MEDIA_PAUSE -> {
				                    runOnUiThread { toggleAiPause() }
				                    return true
				                }
				                KeyEvent.KEYCODE_MUTE,
				                KeyEvent.KEYCODE_VOLUME_MUTE -> {
				                    runOnUiThread { toggleMicMute() }
				                    return true
				                }
				            }
				        }
				    }
				    return super.onMediaButtonEvent(mediaButtonIntent)
				}
            })
            isActive = true
        }
        updateMediaSessionState()
    }

    private fun updateMediaSessionState() {
        val state = if (isAiPaused || !isConnected) PlaybackState.STATE_PAUSED else PlaybackState.STATE_PLAYING
        
        // Provide a simulated moving position when playing to help the OS recognize activity
        val position = if (state == PlaybackState.STATE_PLAYING) System.currentTimeMillis() else PlaybackState.PLAYBACK_POSITION_UNKNOWN
        
        val playbackState = PlaybackState.Builder()
            .setActions(PlaybackState.ACTION_PLAY or PlaybackState.ACTION_PAUSE or PlaybackState.ACTION_PLAY_PAUSE)
            .setState(state, position, 1.0f)
            .build()
        mediaSession?.setPlaybackState(playbackState)
    }

    private fun setupSpeechRecognizer() {
        if (SpeechRecognizer.isRecognitionAvailable(this)) {
            speechRecognizer = SpeechRecognizer.createSpeechRecognizer(this)
            nativeSttIntent = Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH).apply {
                putExtra(RecognizerIntent.EXTRA_LANGUAGE_MODEL, RecognizerIntent.LANGUAGE_MODEL_FREE_FORM)
                putExtra(RecognizerIntent.EXTRA_PARTIAL_RESULTS, true)
                putExtra(RecognizerIntent.EXTRA_MAX_RESULTS, 1)
                
                // Use STT-optimized mic preset (prevents aggressive VOIP noise-gating from swallowing words)
                putExtra("android.speech.extra.AUDIO_SOURCE", MediaRecorder.AudioSource.VOICE_RECOGNITION)
                // Request continuous dictation mode (respected by many OEMs)
                putExtra("android.speech.extra.DICTATION_MODE", true)
                // Force the engine to wait much longer before deciding a sentence is "complete"
                putExtra(RecognizerIntent.EXTRA_SPEECH_INPUT_POSSIBLY_COMPLETE_SILENCE_LENGTH_MILLIS, 2000L)
                putExtra(RecognizerIntent.EXTRA_SPEECH_INPUT_COMPLETE_SILENCE_LENGTH_MILLIS, 3000L)
                putExtra(RecognizerIntent.EXTRA_SPEECH_INPUT_MINIMUM_LENGTH_MILLIS, 5000L)
            }

            speechRecognizer?.setRecognitionListener(object : RecognitionListener {
                override fun onReadyForSpeech(params: Bundle?) { 
                    if (!statusText.startsWith("Heard:")) {
                        statusText = "Listening (Native)..." 
                    }
                }
                
                override fun onBeginningOfSpeech() {
                    statusText = "Recording (Speaking)..."
                }
                override fun onRmsChanged(rmsdB: Float) {
                    if (isAppInForeground) {
                    // Map native dB (-2 to 10) to our visualizer's linear RMS (0 to ~4000)
                        currentRms = ((rmsdB + 2f) / 12f).coerceIn(0f, 1f) * 2500f
                    }
                }
                override fun onBufferReceived(buffer: ByteArray?) {}
                override fun onEndOfSpeech() {} // Let the "Hearing: ..." live text linger until results arrive!
                override fun onError(error: Int) {
                    // Error 5 (CLIENT) means we intentionally cancelled it.
                    // Error 8 (BUSY) means we tried to start it too fast.
                    if (error == SpeechRecognizer.ERROR_CLIENT) return
                    
                    // False alarm! If we suspended TTS for a partial result, resume it!
                    if ((isGeneratingAi || playbackProgress > 0f) && !isAiPaused) {
                        audioEngine.resumePlayback()
                    }

                    lifecycleScope.launch {
                        if (error == SpeechRecognizer.ERROR_RECOGNIZER_BUSY) {
                            delay(500) // Back off slightly more if it's choking
                        }
                        delay(200) // Small backoff to prevent CPU-pegging restart loops
                        restartNativeListening()
                    }
                }
                override fun onResults(results: Bundle?) {
                    val matches = results?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION)
                    if (!matches.isNullOrEmpty() && matches[0].isNotBlank()) {
                        val text = matches[0]
                        Log.d("SpeaxNative", "Native STT Final: $text")
                        
                        statusText = "Heard: $text"
                        lifecycleScope.launch {
                            delay(3000)
                            // Only revert if a new state (like AI speaking or user talking again) hasn't already taken over
                            if (statusText.startsWith("Heard:")) {
                                statusText = "Listening (Native)..."
                            }
                        }

                        if ((isGeneratingAi || playbackProgress > 0f) && !isAiPaused) {
                            Log.d("SpeaxNative", "Native STT Barge-in (Final)! Aborting TTS.")
                            audioEngine.abortPlayback()
                        }

                        sendTextPrompt(text)
                    }
                    lifecycleScope.launch {
                        delay(100)
                        restartNativeListening()
                    }
                }
                override fun onPartialResults(partialResults: Bundle?) {
                    val matches = partialResults?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION)
                    if (!matches.isNullOrEmpty() && matches[0].isNotBlank()) {
                        val partialText = matches[0]
                        statusText = "Hearing: $partialText" // Stream live to the UI!
                        
                        // True Barge-In: We successfully decoded actual words, so kill the AI playback!
                        if ((isGeneratingAi || playbackProgress > 0f) && !isAiPaused) {
                            Log.d("SpeaxNative", "Native STT Barge-in (Partial)! Suspending TTS.")
                            audioEngine.suspendPlayback()
                        }
                    }
                }
                override fun onEvent(eventType: Int, params: Bundle?) {}
            })
        }
    }

    private fun restartNativeListening() {
        if (isConnected && !isMicMuted && useNativeStt) {
            runOnUiThread {
                speechRecognizer?.cancel()
                speechRecognizer?.startListening(nativeSttIntent)
            }
        }
    }

    private fun stopNativeListening() {
        runOnUiThread {
            speechRecognizer?.cancel()
            currentRms = 0f
        }
    }

    fun swapSttEngine(useNative: Boolean) {
        useNativeStt = useNative
        saveSettingsLocal()
        
        if (isConnected && !isMicMuted) {
            if (useNative) {
                audioEngine.stopRecording()
                restartNativeListening()
            } else {
                stopNativeListening()
                audioEngine.startRecording()
            }
        }
    }

    fun saveSettingsLocal() {
        SpeaxManager.saveSettingsLocal()
    }

	fun pushSettingsToServer() {
        SpeaxManager.pushSettingsToServer()
    }

    fun fetchModels() {
        SpeaxManager.fetchModels()
    }
    
    fun fetchVoices() {
        SpeaxManager.fetchVoices()
    }

    fun toggleConnection() {
        if (isConnected) {
            disconnectWebSocket()
        } else {
            connectWebSocket()
        }
    }

    private fun connectWebSocket() {
        SpeaxManager.connect(mediaSession?.sessionToken)
        
        // Ask the Go server for the latest settings, threads, and history
        SpeaxManager.sendTextPrompt("[REQUEST_SYNC]")

        if (useNativeStt) {
            restartNativeListening()
        } 
        updateBackgroundService()
        mediaSession?.isActive = true
        updateMediaSessionState()
    }


    fun toggleMicMute() {
        SpeaxManager.toggleMicMute()
        Log.d("SpeaxUI", "Mic Muted State: $isMicMuted")
        
        if (useNativeStt) {
            if (isMicMuted) {
                stopNativeListening()
            } else {
                restartNativeListening()
            }
        }
        
        updateBackgroundService()
        updateWakeLocks()
    }

    fun toggleAiPause() {
        Log.d("SpeaxUI", "toggleAiPause: currently isAiPaused=$isAiPaused")
        SpeaxManager.toggleAiPause()
        
        // Match the legacy behavior where pausing mutes the mic
        if (isAiPaused) {
            isMicMuted = true
            audioEngine.isMicMuted = true
            if (useNativeStt) stopNativeListening()
        } else {
            isMicMuted = false
            audioEngine.isMicMuted = false
            if (useNativeStt) restartNativeListening()
        }

        updateBackgroundService()
        updateMediaSessionState()
        updateWakeLocks()
    }


    private fun disconnectWebSocket() {
        SpeaxManager.disconnect()
        audioEngine.abortPlayback()
        stopNativeListening()
        audioEngine.stopRecording()
        mediaSession?.isActive = false
        updateMediaSessionState()
        updateWakeLocks()

    }
    
    fun logout() {
        disconnectWebSocket()
        getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().clear().apply()
        CookieManager.getInstance().removeAllCookies(null)
        sessionCookie = null
    }

    fun switchThread(id: String) = SpeaxManager.switchThread(id)
    fun deleteThread(id: String) = SpeaxManager.deleteThread(id)
    fun newThread(name: String) = SpeaxManager.newThread(name)
    fun renameThread(name: String) = SpeaxManager.renameThread(name)
    fun sendTextPrompt(text: String) = SpeaxManager.sendTextPrompt(text)
    fun sendTypedPrompt(text: String) = SpeaxManager.sendTypedPrompt(text)
    fun deleteMessage(index: Int) = SpeaxManager.deleteMessage(index)
    fun deleteMessagePair(index: Int) = SpeaxManager.deleteMessagePair(index)
    fun clearHistory() = SpeaxManager.clearHistory()
    fun rebuildSummary() = SpeaxManager.rebuildSummary()

    override fun onDestroy() {
        super.onDestroy()
	    unregisterReceiver(hardwareButtonReceiver)
        audioEngine.stopRecording()
        audioEngine.release()
        disconnectWebSocket()
        speechRecognizer?.destroy()
        mediaSession?.release()
    }
}



@Composable
fun LoginScreen(authUrl: String) {
    val mainActivity = androidx.compose.ui.platform.LocalContext.current as MainActivity
    
    Box(modifier = Modifier.fillMaxSize().systemBarsPadding(), contentAlignment = Alignment.Center) {
        Button(onClick = {
            val builder = CustomTabsIntent.Builder()
            val customTabsIntent = builder.build()
            customTabsIntent.launchUrl(mainActivity, Uri.parse(authUrl))
        }) {
            Text("Login with Google")
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun ChatScreen() {
    val listState = rememberLazyListState()
    val mainActivity = androidx.compose.ui.platform.LocalContext.current as MainActivity
    val drawerState = rememberDrawerState(initialValue = DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    var expandedModelDropdown by remember { mutableStateOf(false) }
    var expandedVoiceDropdown by remember { mutableStateOf(false) }
    var selectedTab by remember { mutableStateOf(0) }
    
    // Thread Modal State
    var showThreadsSheet by remember { mutableStateOf(false) }
    var showNewThreadDialog by remember { mutableStateOf(false) }
    var showRenameDialog by remember { mutableStateOf(false) }
    var newThreadName by remember { mutableStateOf("") }

        val isImeVisible = WindowInsets.isImeVisible
    val isDark = isSystemInDarkTheme()
    val bgGradient = if (isDark) {
        Brush.verticalGradient(listOf(Color(0xFF051329), Color(0xFF010308)))
    } else {
        Brush.verticalGradient(listOf(Color(0xFFE3EDF7), Color(0xFFF5F7FA))) // Airy light gradient
    }

        // Auto-scroll to bottom when new text arrives or the keyboard opens
        LaunchedEffect(mainActivity.messages.size, mainActivity.messages.lastOrNull()?.content?.length, isImeVisible) {
        if (mainActivity.messages.isNotEmpty()) {
            listState.scrollToItem(mainActivity.messages.size - 1)
        }
    }

    // Fetch models once when the UI boots
    LaunchedEffect(Unit) {
        mainActivity.fetchModels()
        mainActivity.fetchVoices()
    }

    ModalNavigationDrawer(
        drawerState = drawerState,
        scrimColor = Color.Black.copy(alpha = 0.6f),
        drawerContent = {
            ModalDrawerSheet(
                modifier = Modifier.width(300.dp),
                drawerContainerColor = MaterialTheme.colorScheme.background
            ) {
                Spacer(Modifier.height(16.dp))
                Text("Settings", modifier = Modifier.padding(16.dp), color = MaterialTheme.colorScheme.secondary, fontWeight = FontWeight.Bold, fontSize = 20.sp)
                Divider(color = MaterialTheme.colorScheme.outline)
                
                Column(modifier = Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(16.dp)) {
                    // --- Settings UI ---
                    OutlinedTextField(
                        value = mainActivity.userName,
                        onValueChange = { mainActivity.userName = it; mainActivity.pushSettingsToServer() },
                        label = { Text("Your Name") },
                        modifier = Modifier.fillMaxWidth(),
                        singleLine = true
                    )
                    Spacer(Modifier.height(12.dp))
                    OutlinedTextField(
                        value = mainActivity.userBio,
                        onValueChange = { mainActivity.userBio = it; mainActivity.pushSettingsToServer() },
                        label = { Text("System Bio / Role") },
                        modifier = Modifier.fillMaxWidth(),
                        maxLines = 3
                    )
                    Spacer(Modifier.height(24.dp))

                    // Mic Profile Dropdown
                    var expandedMicDropdown by remember { mutableStateOf(false) }
                    val micProfiles = listOf(
                        "sensitive" to "Sensitive (Low Filtering)",
                        "standard" to "Standard", 
                        "adaptive" to "Adaptive Filtering", 
                        "heavy" to "Heavy Filtering", 
                        "mute_playback" to "Mute on Playback"
                    )
                    val currentMicProfileLabel = micProfiles.find { it.first == mainActivity.micProfile }?.second ?: "Standard"

                    ExposedDropdownMenuBox(
                        expanded = expandedMicDropdown,
                        onExpandedChange = { expandedMicDropdown = !expandedMicDropdown }
                    ) {
                        OutlinedTextField(
                            value = currentMicProfileLabel,
                            onValueChange = { },
                            readOnly = true,
                            label = { Text("Mic Profile") },
                            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expandedMicDropdown) },
                            modifier = Modifier.fillMaxWidth().menuAnchor()
                        )
                        ExposedDropdownMenu(
                            expanded = expandedMicDropdown,
                            onDismissRequest = { expandedMicDropdown = false }
                        ) {
                            micProfiles.forEach { (value, label) ->
                                DropdownMenuItem(
                                    text = { Text(label) },
                                    onClick = {
                                        SpeaxManager.updateMicProfile(value)
                                        expandedMicDropdown = false
                                    }
                                )
                            }
                        }
                    }
                    Spacer(Modifier.height(16.dp))

                    // Native STT Toggle
                    if (mainActivity.isNativeSttSupported) {
                        Row(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                            Text("Use Native Device STT", color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
                            Switch(
                                checked = mainActivity.useNativeStt,
                                onCheckedChange = { isNative -> mainActivity.swapSttEngine(isNative) },
                                colors = SwitchDefaults.colors(checkedThumbColor = MaterialTheme.colorScheme.primary, checkedTrackColor = MaterialTheme.colorScheme.primary.copy(alpha = 0.5f))
                            )
                        }
                        Spacer(Modifier.height(16.dp))
                    }

                    // Local TTS Toggle
                    Row(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                        Text("Use Local TTS (Piper)", color = MaterialTheme.colorScheme.onSurface, modifier = Modifier.weight(1f))
                        Switch(
                            checked = mainActivity.useLocalTts,
                            onCheckedChange = { isLocal -> 
                                mainActivity.useLocalTts = isLocal
                                mainActivity.pushSettingsToServer()
                                mainActivity.fetchVoices() 
                            },
                            colors = SwitchDefaults.colors(checkedThumbColor = MaterialTheme.colorScheme.primary, checkedTrackColor = MaterialTheme.colorScheme.primary.copy(alpha = 0.5f))
                        )
                    }
                    Spacer(Modifier.height(16.dp))

                    // Passive Assistant Toggle
                    Row(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                        Column(modifier = Modifier.weight(1f)) {
                            Text("Passive Assistant", color = MaterialTheme.colorScheme.onSurface)
                            Text("Only responds when addressed by name (e.g. ${SpeaxManager.assistantName})", color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f), fontSize = 12.sp)
                        }
                        Switch(
                            checked = mainActivity.passiveAssistant,
                            onCheckedChange = { isPassive -> 
                                mainActivity.passiveAssistant = isPassive
                                mainActivity.pushSettingsToServer()
                            },
                            colors = SwitchDefaults.colors(checkedThumbColor = MaterialTheme.colorScheme.primary, checkedTrackColor = MaterialTheme.colorScheme.primary.copy(alpha = 0.5f))
                        )
                    }
                    Spacer(Modifier.height(16.dp))

                    Text("AI Provider", color = MaterialTheme.colorScheme.onSurface, fontWeight = FontWeight.Bold, fontSize = 14.sp)
                    Row(modifier = Modifier.fillMaxWidth().padding(top = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Button(
                            onClick = { mainActivity.aiProvider = "ollama"; mainActivity.pushSettingsToServer(); mainActivity.fetchModels() },
                            colors = ButtonDefaults.buttonColors(
                                containerColor = if (mainActivity.aiProvider == "ollama") MaterialTheme.colorScheme.primary else Color.DarkGray,
                                contentColor = Color.White
                            ),
                            modifier = Modifier.weight(1f)
                        ) { Text("Ollama") }
                        Button(
                            onClick = { mainActivity.aiProvider = "gemini"; mainActivity.pushSettingsToServer(); mainActivity.fetchModels() },
                            colors = ButtonDefaults.buttonColors(
                                containerColor = if (mainActivity.aiProvider == "gemini") MaterialTheme.colorScheme.primary else Color.DarkGray,
                                contentColor = Color.White
                            ),
                            modifier = Modifier.weight(1f)
                        ) { Text("Gemini") }
                    }
                    Spacer(Modifier.height(12.dp))
                    
                    if (mainActivity.aiProvider == "gemini") {
                        OutlinedTextField(
                            value = mainActivity.geminiApiKey,
                            onValueChange = { mainActivity.geminiApiKey = it; mainActivity.pushSettingsToServer(); mainActivity.fetchModels() },
                            label = { Text("Gemini API Key") },
                            modifier = Modifier.fillMaxWidth(),
                            singleLine = true
                        )
                        Spacer(Modifier.height(12.dp))
                    }
                    
                    ExposedDropdownMenuBox(
                        expanded = expandedModelDropdown,
                        onExpandedChange = { expandedModelDropdown = !expandedModelDropdown }
                    ) {
                        OutlinedTextField(
                            value = if (mainActivity.isLoadingModels) "Loading..." else if (mainActivity.aiModel.isBlank()) "Select Model" else mainActivity.aiModel,
                            onValueChange = { },
                            readOnly = true,
                            label = { Text("Model") },
                            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expandedModelDropdown) },
                            modifier = Modifier.fillMaxWidth().menuAnchor()
                        )
                        ExposedDropdownMenu(
                            expanded = expandedModelDropdown,
                            onDismissRequest = { expandedModelDropdown = false }
                        ) {
                            if (mainActivity.availableModels.isEmpty() && !mainActivity.isLoadingModels) {
                                DropdownMenuItem(text = { Text("No models found") }, onClick = { expandedModelDropdown = false })
                            } else {
                                mainActivity.availableModels.forEach { selectionOption ->
                                    DropdownMenuItem(
                                        text = { Text(selectionOption) },
                                        onClick = {
                                            mainActivity.aiModel = selectionOption
                                            //mainActivity.saveSettings()
                                            mainActivity.pushSettingsToServer()
                                            expandedModelDropdown = false
                                        }
                                    )
                                }
                            }
                        }
                    }
                    Spacer(Modifier.height(12.dp))

                    ExposedDropdownMenuBox(
                        expanded = expandedVoiceDropdown,
                        onExpandedChange = { expandedVoiceDropdown = !expandedVoiceDropdown }
                    ) {
                        OutlinedTextField(
                            value = if (mainActivity.isLoadingVoices) "Loading..." else if (mainActivity.aiVoice.isBlank()) "Select Voice" else SpeaxManager.cleanVoiceName(mainActivity.aiVoice),
                            onValueChange = { },
                            readOnly = true,
                            label = { Text("Voice") },
                            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expandedVoiceDropdown) },
                            modifier = Modifier.fillMaxWidth().menuAnchor()
                        )
                        ExposedDropdownMenu(
                            expanded = expandedVoiceDropdown,
                            onDismissRequest = { expandedVoiceDropdown = false }
                        ) {
                            if (mainActivity.availableVoices.isEmpty() && !mainActivity.isLoadingVoices) {
                                DropdownMenuItem(text = { Text("No voices found") }, onClick = { expandedVoiceDropdown = false })
                            } else {
                                mainActivity.availableVoices.forEach { selectionOption ->
                                    DropdownMenuItem(
                                        text = { Text(SpeaxManager.cleanVoiceName(selectionOption)) },
                                        onClick = {
                                            mainActivity.aiVoice = "$selectionOption.onnx"
                                            //mainActivity.saveSettings()
                                            mainActivity.pushSettingsToServer()
                                            expandedVoiceDropdown = false
                                        }
                                    )
                                }
                            }
                        }
                    }

                    Spacer(Modifier.height(24.dp))
                    Divider(color = MaterialTheme.colorScheme.outline)
                    Spacer(Modifier.height(24.dp))
                }

                Divider(color = MaterialTheme.colorScheme.outline)
                
                NavigationDrawerItem(
                    label = { Text("Logout", color = Color(0xFFD16969)) },
                    selected = false,
                    onClick = { mainActivity.logout() },
                    modifier = Modifier.padding(16.dp)
                )
            }
        }
    ) {
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(bgGradient)
        ) {
            Scaffold(
                contentWindowInsets = WindowInsets.systemBars,
                topBar = {
                    TopAppBar(
                        title = { 
                            TextButton(onClick = { newThreadName = mainActivity.activeThreadName; showRenameDialog = true }) {
                                Text(mainActivity.activeThreadName, fontWeight = FontWeight.Bold, fontSize = 18.sp, color = MaterialTheme.colorScheme.onBackground)
                            }
                        },
                        colors = TopAppBarDefaults.topAppBarColors(
                            containerColor = Color.Transparent,
                            titleContentColor = MaterialTheme.colorScheme.onBackground
                        ),
                        navigationIcon = {
                            TextButton(onClick = { scope.launch { drawerState.open() } }) {
                                Text("☰", fontSize = 24.sp, color = MaterialTheme.colorScheme.onBackground)
                            }
                        },
                        actions = {
                            IconButton(onClick = { /* Syncs with web app, no dropdown here */ }) {
                                Icon(Icons.Filled.Person, contentDescription = "Profile", tint = MaterialTheme.colorScheme.onBackground)
                            }
                        }
                    )
                },
                bottomBar = {
                    if (!isImeVisible) {
                        Box(modifier = Modifier.padding(horizontal = 12.dp, vertical = 12.dp)) {
                            NavigationBar(
                                modifier = Modifier
                                    .clip(RoundedCornerShape(8.dp))
                                    .border(1.dp, MaterialTheme.colorScheme.outline, RoundedCornerShape(8.dp)),
                                containerColor = MaterialTheme.colorScheme.surface,
                                tonalElevation = 0.dp
                            ) {
                                val tabs = listOf("Home", "Thread", "Memory")
                                val tabIcons = listOf(Icons.Filled.Home, Icons.AutoMirrored.Filled.List, Icons.Filled.Memory)
                                tabs.forEachIndexed { index, title ->
                                    NavigationBarItem(
                                        icon = { Icon(tabIcons[index], contentDescription = title) },
                                        label = { Text(title, fontSize = 14.sp) },
                                        selected = selectedTab == index,
                                        onClick = { selectedTab = index },
                                        colors = NavigationBarItemDefaults.colors(
                                            selectedTextColor = MaterialTheme.colorScheme.primary,
                                            unselectedTextColor = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f),
                                            selectedIconColor = MaterialTheme.colorScheme.primary,
                                            unselectedIconColor = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.5f),
                                            indicatorColor = MaterialTheme.colorScheme.surfaceVariant
                                        )
                                    )
                                }
                            }
                        }
                    }
                },
                floatingActionButton = {
                    if (!isImeVisible) {
                        FloatingActionButton(
                            onClick = { showThreadsSheet = true },
                            containerColor = MaterialTheme.colorScheme.surfaceVariant,
                            contentColor = MaterialTheme.colorScheme.onSurface
                        ) {
                            Icon(Icons.AutoMirrored.Filled.Chat, contentDescription = "Threads")
                        }
                    }
                },
                containerColor = Color.Transparent
            ) { innerPadding ->
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(innerPadding)
                        .consumeWindowInsets(innerPadding)
                        .imePadding()
                    .padding(horizontal = 12.dp)
                    .background(MaterialTheme.colorScheme.surface, RoundedCornerShape(8.dp))
                    .border(1.dp, MaterialTheme.colorScheme.outline, RoundedCornerShape(8.dp))
                    .clip(RoundedCornerShape(8.dp))
            ) {
                Column(modifier = Modifier.fillMaxSize()) {
                    // Persistent status bar — visible on all tabs, matching web app.js
                    Text(
                        text = "Status: ${mainActivity.statusText}",
                        color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f),
                        fontSize = 12.sp,
                        modifier = Modifier
                            .fillMaxWidth()
                            .background(MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f))
                            .padding(horizontal = 12.dp, vertical = 6.dp)
                    )
                    
                    PlaybackProgressBar(
                        mainActivity = mainActivity,
                        modifier = Modifier.fillMaxWidth().height(4.dp)
                    )

                when (selectedTab) {
                    0 -> HomeTab(mainActivity)
                    1 -> ThreadTab(mainActivity, listState)
                    2 -> MemoryTab(mainActivity)
                }

        // Threads Bottom Sheet Overlay
        if (showThreadsSheet) {
            ModalBottomSheet(
                onDismissRequest = { showThreadsSheet = false },
                scrimColor = Color.Black.copy(alpha = 0.6f),
                containerColor = MaterialTheme.colorScheme.surface
            ) {
                Column(modifier = Modifier.padding(16.dp)) {
                    Text("Your Threads", fontSize = 20.sp, fontWeight = FontWeight.Bold, color = MaterialTheme.colorScheme.secondary)
                    Spacer(modifier = Modifier.height(16.dp))
                    Button(onClick = { newThreadName = ""; showNewThreadDialog = true }, modifier = Modifier.fillMaxWidth()) {
                        Text("+ New Thread")
                    }
                    Spacer(modifier = Modifier.height(16.dp))
                    LazyColumn {
                        items(mainActivity.availableThreads) { t ->
                            Row(
                                modifier = Modifier.fillMaxWidth().padding(vertical = 8.dp),
                                verticalAlignment = Alignment.CenterVertically
                            ) {
                                TextButton(
                                    onClick = { mainActivity.switchThread(t.id); showThreadsSheet = false },
                                    modifier = Modifier.weight(1f)
                                ) {
                                    Text(
                                        text = t.name,
                                                    color = if (t.id == mainActivity.activeThreadId) MaterialTheme.colorScheme.secondary else MaterialTheme.colorScheme.onSurface,
                                        modifier = Modifier.fillMaxWidth()
                                    )
                                }
                                TextButton(onClick = { mainActivity.deleteThread(t.id) }) {
                                    Text("X", color = Color(0xFFD16969), fontWeight = FontWeight.Bold)
                                }
                            }
                        }
                    }
                    Spacer(modifier = Modifier.height(48.dp)) // Padding for bottom edge
                }
            }
        }

        // New / Rename Thread Dialog
        if (showNewThreadDialog || showRenameDialog) {
            AlertDialog(
                onDismissRequest = { showNewThreadDialog = false; showRenameDialog = false },
                title = { Text(if (showRenameDialog) "Rename Thread" else "New Thread") },
                text = {
                    OutlinedTextField(
                        value = newThreadName,
                        onValueChange = { newThreadName = it },
                        label = { Text("Thread Name") },
                        singleLine = true
                    )
                },
                confirmButton = {
                    TextButton(onClick = {
                        if (showRenameDialog) mainActivity.renameThread(newThreadName) else mainActivity.newThread(newThreadName)
                        showNewThreadDialog = false
                        showRenameDialog = false
                    }) { Text(if (showRenameDialog) "Save" else "Create") }
                },
                dismissButton = {
                    TextButton(onClick = { showNewThreadDialog = false; showRenameDialog = false }) { Text("Cancel") }
                }
            )
        }
                } // Closes Column wrapping status bar + tab content
    } // Closes Box
} // Closes Outer Box for Gradient
    } // Closes Scaffold
} // Closes ModalNavigationDrawer
} // Closes ChatScreen

@Composable
fun PlaybackProgressBar(mainActivity: MainActivity, modifier: Modifier = Modifier) {
    var animatedProgress by remember { mutableFloatStateOf(0f) }

    LaunchedEffect(Unit) {
        while (true) {
            androidx.compose.runtime.withFrameNanos {
                val targetProgress = mainActivity.playbackProgress.coerceIn(0f, 1f)
                if (targetProgress == 0f) {
                    animatedProgress = 0f // Snap instantly to 0 when audio finishes or aborts
                } else {
                    animatedProgress += (targetProgress - animatedProgress) * 0.1f // Smooth lerp forward
                }
            }
        }
    }

    Box(modifier = modifier.background(MaterialTheme.colorScheme.background)) {
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(fraction = animatedProgress)
                .background(androidx.compose.ui.graphics.Brush.horizontalGradient(listOf(MaterialTheme.colorScheme.primary, MaterialTheme.colorScheme.secondary)))
        )
    }
}

