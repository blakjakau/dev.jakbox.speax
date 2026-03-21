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
import com.k2fsa.sherpa.onnx.OfflineTts
import com.k2fsa.sherpa.onnx.OfflineTtsConfig
import com.k2fsa.sherpa.onnx.OfflineTtsModelConfig
import com.k2fsa.sherpa.onnx.OfflineTtsVitsModelConfig
import android.content.BroadcastReceiver
import android.content.IntentFilter

data class UiMessage(val role: String, val content: String)
data class ThreadItem(val id: String, val name: String)

class MainActivity : ComponentActivity() {

    private var speaxWebSocket: SpeaxWebSocket? = null
    lateinit var audioEngine: AudioEngine
    private val httpClient = OkHttpClient()
    private var mediaSession: MediaSession? = null

    // State variables that Compose will observe to update the UI instantly
    val messages = mutableStateListOf<UiMessage>()
    var isConnected by mutableStateOf(false)
    var statusText by mutableStateOf("Disconnected")
    private var isGeneratingAi = false
    var currentRms by mutableStateOf(0f)
    var aiRms by mutableStateOf(0f)
    var playbackProgress by mutableStateOf(0f)
    var isMicMuted by mutableStateOf(false)
    var isAiPaused by mutableStateOf(false)
    private var wakeLock: PowerManager.WakeLock? = null
    private val focusChangeListener = AudioManager.OnAudioFocusChangeListener { _ -> }
    private var audioFocusRequest: Any? = null

    // Settings State
    var aiProvider by mutableStateOf("ollama")
    var geminiApiKey by mutableStateOf("")
    var aiModel by mutableStateOf("")
    var aiVoice by mutableStateOf("")
    var availableModels = mutableStateListOf<String>()
    var isLoadingModels by mutableStateOf(false)
    var availableVoices = mutableStateListOf<String>()
    var isLoadingVoices by mutableStateOf(false)
    var userName by mutableStateOf("")
    var userBio by mutableStateOf("")
    var googleName by mutableStateOf("")
    var useNativeStt by mutableStateOf(false)
    var isNativeSttSupported by mutableStateOf(false)
    var useLocalTts by mutableStateOf(false)
    var passiveAssistant by mutableStateOf(false)
    var micProfile by mutableStateOf("standard")

    // Memory & Thread State
    var memorySummary by mutableStateOf("No summary generated yet.")
    var activeThreadId by mutableStateOf("default")
    var archiveTurns by mutableStateOf(0)
    var maxArchiveTurns by mutableStateOf(100)
    var estTokens by mutableStateOf(0)
    var maxTokens by mutableStateOf(8192)
    val tokenUsage = mutableStateMapOf<String, Long>()
    var activeThreadName by mutableStateOf("General Chat")
    val availableThreads = mutableStateListOf<ThreadItem>()

    // Auth State
    var sessionCookie by mutableStateOf<String?>(null)

    // Native STT
    private var speechRecognizer: SpeechRecognizer? = null
    private var nativeSttIntent: Intent? = null

    private var isAppInForeground = false
    private var cpuWakeLock: PowerManager.WakeLock? = null

    private lateinit var piperEngine: PiperEngine
    private val piperMutex = Mutex() // Ensures we only synthesize one sentence at a time (saves CPU)

    private val leadingCommaRe = Regex(",\\s+([A-Z])")
    private val mdRe = Regex("[*_`#~]")
    private val wsRe = Regex("\\s{2,}")

    private fun sanitiseTTSText(text: String): String {
        var processedText = text
        processedText = leadingCommaRe.replace(processedText, " $1")
        processedText = mdRe.replace(processedText, "")
        processedText = wsRe.replace(processedText, " ")
        return processedText.trim()
    }
	
	private val serverUrl = "wss://speax.jakbox.dev/ws"
    private val authUrl = "https://speax.jakbox.dev/auth/login?client=android"

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Enable edge-to-edge so Compose can detect and measure the keyboard (IME)
        WindowCompat.setDecorFitsSystemWindows(window, false)
        
        // Tell legacy Android to STOP resizing the window, letting Compose's .imePadding() do 100% of the work
        window.setSoftInputMode(android.view.WindowManager.LayoutParams.SOFT_INPUT_ADJUST_NOTHING)

        // Load saved session if it exists
        val prefs = getSharedPreferences("speax_prefs", Context.MODE_PRIVATE)
        sessionCookie = prefs.getString("session_cookie", null)
        aiProvider = prefs.getString("provider", "ollama") ?: "ollama"
        geminiApiKey = prefs.getString("api_key", "") ?: ""
        aiModel = prefs.getString("model", "") ?: ""
        aiVoice = prefs.getString("voice", "") ?: ""
        userName = prefs.getString("user_name", "") ?: ""
        userBio = prefs.getString("user_bio", "") ?: ""
        googleName = prefs.getString("google_name", "") ?: ""
        
        isNativeSttSupported = SpeechRecognizer.isRecognitionAvailable(this)
        useNativeStt = prefs.getBoolean("use_native_stt", isNativeSttSupported) // Default to true if hardware supports it
        useLocalTts = prefs.getBoolean("use_local_tts", false)
        passiveAssistant = prefs.getBoolean("passive_assistant", false)
        micProfile = prefs.getString("mic_profile", "standard") ?: "standard"

        // 0. Initialize Edge TTS Pipeline
        piperEngine = PiperEngine(this)
        lifecycleScope.launch(Dispatchers.IO) {
            piperEngine.initEngine()
        }

        // 1. Initialize Audio Engine
        audioEngine = AudioEngine(onSpeechFinalized = { _, _ -> }, onVolumeChange = { rms ->
            // Pass RMS back to UI thread for Visualizer scaling
            if (isAppInForeground) runOnUiThread { currentRms = rms }
        }, onSpeechStart = {
            // Instant UI feedback that VAD tripped
            runOnUiThread { statusText = "Recording (Speaking)..." }
        }, onAiVolumeChange = { rms ->
            if (isAppInForeground) runOnUiThread { aiRms = rms }
        }, onBufferProgress = { progress ->
            if (isAppInForeground) runOnUiThread { playbackProgress = progress }
        }, onPlaybackComplete = {
            speaxWebSocket?.sendText("[PLAYBACK_COMPLETE]")
        }, onStreamingChunk = { pcmData, seqID, type ->
            speaxWebSocket?.sendStreamingChunk(type, seqID, pcmData)
            if (type == 0x02.toByte()) {
                runOnUiThread { statusText = "Processing with AI..." }
            }
        })
        audioEngine.micProfile = micProfile

        // 2. Request Mic & Notification Permissions
        val requestPermissionLauncher = registerForActivityResult(
            ActivityResultContracts.RequestMultiplePermissions()
        ) { permissions ->
            if (permissions[Manifest.permission.RECORD_AUDIO] == false) {
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
		
		// Register the local receiver so we can use mute/playpause hardware
	    val filter = IntentFilter("SPEAX_HARDWARE_BTN")
	    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
	        registerReceiver(hardwareButtonReceiver, filter, Context.RECEIVER_NOT_EXPORTED)
	    } else {
	        registerReceiver(hardwareButtonReceiver, filter)
	    }
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

        // CPU wake: keep the CPU alive whenever mic is recording (even if screen is off)
        if (micActive) {
            if (cpuWakeLock == null) {
                val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
                cpuWakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "speax:mic_active")
                cpuWakeLock?.acquire()
                Log.d("WakeLock", "PARTIAL_WAKE_LOCK acquired")
            }
        } else {
            cpuWakeLock?.let {
                if (it.isHeld) {
                    it.release()
                    Log.d("WakeLock", "PARTIAL_WAKE_LOCK released")
                }
            }
            cpuWakeLock = null
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
	    val intent = Intent(this, SpeaxService::class.java)
	    if (isConnected) {
	        intent.action = "START"
	        // Pass the token to the background service
	        mediaSession?.sessionToken?.let { token ->
	            intent.putExtra("session_token", token)
	        }
	        // Pass the current mute/pause state to the notification
	        intent.putExtra("is_muted", isMicMuted || isAiPaused)
	        
	        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
	            startForegroundService(intent)
	        } else {
	            startService(intent)
	        }
	    } else {
	        intent.action = "STOP"
	        startService(intent)
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
        getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().apply {
            putString("provider", aiProvider)
            putString("api_key", geminiApiKey)
            putString("model", aiModel)
            putString("voice", aiVoice)
            putString("user_name", userName)
            putString("user_bio", userBio)
            putString("google_name", googleName)
            putBoolean("use_native_stt", useNativeStt)
            putBoolean("use_local_tts", useLocalTts)
            putBoolean("passive_assistant", passiveAssistant)
        }.apply()
	}

	fun pushSettingsToServer() {
        saveSettingsLocal()
        val settingsJson = JSONObject().apply {
            put("userName", userName)
            put("googleName", googleName)
            put("userBio", userBio)
            put("provider", aiProvider)
            put("apiKey", geminiApiKey)
            put("model", aiModel)
            put("voice", aiVoice)
            put("clientStorage", false)
            put("clientTts", useLocalTts)
            put("passiveAssistant", passiveAssistant)
        }
        speaxWebSocket?.sendText("[SETTINGS]$settingsJson")
    }

    fun fetchModels() {
        if (aiProvider == "gemini" && geminiApiKey.isBlank()) {
            availableModels.clear()
            return
        }
        isLoadingModels = true
        val url = "https://speax.jakbox.dev/api/models?provider=$aiProvider&apiKey=$geminiApiKey"
        
        lifecycleScope.launch(Dispatchers.IO) {
            try {
                val request = Request.Builder().url(url).build()
                val response = httpClient.newCall(request).execute()
                if (response.isSuccessful) {
                    val jsonStr = response.body?.string() ?: "[]"
                    val jsonArray = org.json.JSONArray(jsonStr)
                    val models = mutableListOf<String>()
                    for (i in 0 until jsonArray.length()) {
                        models.add(jsonArray.getJSONObject(i).getString("id"))
                    }
                    withContext(Dispatchers.Main) {
                        availableModels.clear()
                        availableModels.addAll(models)
                    }
                }
            } catch (e: Exception) { Log.e("MainActivity", "Error fetching models", e) }
            finally { withContext(Dispatchers.Main) { isLoadingModels = false } }
        }
    }
    
    fun fetchVoices() {
        val isLocal = useLocalTts // Capture state synchronously before background thread
        Log.d("SpeaxVoices", "fetchVoices triggered! isLocalTTS=$isLocal")      
        isLoadingVoices = true
        
        lifecycleScope.launch(Dispatchers.IO) {
            try {
                val voices = mutableListOf<String>()
                
                if (isLocal) {
                    // Wait for PiperEngine to finish extracting assets on first boot!
                    var retries = 0
                    while (!piperEngine.isReady && retries < 40) {
                        kotlinx.coroutines.delay(250)
                        retries++
                    }

                    // Recursively scan local device storage for .onnx models
                    val piperDir = File(filesDir, "piper_env")
                    val onnxFiles = piperDir.walkTopDown()
                        .filter { it.isFile && it.name.endsWith(".onnx") }
                        .toList()
					Log.d("SpeaxVoices", "Local scan complete. Found ${onnxFiles.size} models: ${onnxFiles.map { it.name }}")
                    voices.addAll(onnxFiles.map { it.name.replace(".onnx", "") }.distinct())
                } else {
                    Log.d("SpeaxVoices", "Fetching remote models from server...")
                    // Fetch available models from the remote Go Server
                    val url = "https://speax.jakbox.dev/api/voices"
                    val request = Request.Builder().url(url).build()
                    val response = httpClient.newCall(request).execute()
                    if (response.isSuccessful) {
                        val jsonStr = response.body?.string() ?: "[]"
                        val jsonArray = org.json.JSONArray(jsonStr)
                        for (i in 0 until jsonArray.length()) {
                            voices.add(jsonArray.getString(i).replace(".onnx", ""))
                        }
                    }
                }
                
                withContext(Dispatchers.Main) {
                    availableVoices.clear()
                    availableVoices.addAll(voices)
                }
            } catch (e: Exception) { Log.e("MainActivity", "Error fetching voices", e) }
            finally { withContext(Dispatchers.Main) { isLoadingVoices = false } }
        }
    }

    fun toggleConnection() {
        if (isConnected) {
            disconnectWebSocket()
        } else {
            connectWebSocket()
        }
    }

    private fun connectWebSocket() {
        // Force Android into VoIP mode to enable the aggressive hardware echo cancellation 
        // and noise suppression typically reserved for phone calls (matching Chrome WebRTC).
        val audioManager = getSystemService(Context.AUDIO_SERVICE) as AudioManager
        audioManager.mode = AudioManager.MODE_IN_COMMUNICATION

        // Request Audio Focus to guarantee our MediaSession receives hardware button events
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val request = android.media.AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN)
                .setAudioAttributes(
                    android.media.AudioAttributes.Builder()
                        .setUsage(android.media.AudioAttributes.USAGE_VOICE_COMMUNICATION)
                        .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build()
                )
                .setOnAudioFocusChangeListener(focusChangeListener)
                .build()
            audioFocusRequest = request
            audioManager.requestAudioFocus(request)
        } else {
            @Suppress("DEPRECATION")
            audioManager.requestAudioFocus(focusChangeListener, AudioManager.STREAM_VOICE_CALL, AudioManager.AUDIOFOCUS_GAIN)
        }

        // Force audio routing to Bluetooth Headset mic if available!
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val devices = audioManager.availableCommunicationDevices
            val btDevice = devices.firstOrNull { 
                it.type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO || it.type == AudioDeviceInfo.TYPE_BLE_HEADSET 
            }
            if (btDevice != null) {
                audioManager.setCommunicationDevice(btDevice)
            }
        } else {
            @Suppress("DEPRECATION")
            audioManager.startBluetoothSco()
            @Suppress("DEPRECATION")
            audioManager.isBluetoothScoOn = true
        }

        // Acquire Partial WakeLock to keep CPU running for continuous STT
        val powerManager = getSystemService(Context.POWER_SERVICE) as PowerManager
        wakeLock = powerManager.newWakeLock(
            PowerManager.PARTIAL_WAKE_LOCK,
            "Speax::AudioLock"
        ).apply { acquire() }

        val deviceName = java.net.URLEncoder.encode("${Build.MANUFACTURER} ${Build.MODEL}", "UTF-8")
        val fullUrl = "$serverUrl?client=android&device=$deviceName"

        statusText = "Connecting..."
        speaxWebSocket = SpeaxWebSocket(
            url = fullUrl,
            cookie = sessionCookie ?: "",
            onTextReceived = { text ->
                // OkHttp calls this on a background thread. Keep the JSON parsing here!
                handleIncomingText(text)
            },
            onAudioReceived = { audioBytes ->
                Log.d("SpeaxClient", "RX Audio: ${audioBytes.size} bytes")
                audioEngine.playAudioChunk(audioBytes)
            },
            onClosed = {
                if (isConnected) {
                    attemptReconnect()
                }
            }
        )
        speaxWebSocket?.connect()
        
        // Ask the Go server for the latest settings, threads, and history
        // The server is the single source of truth for cross-device sessions
        speaxWebSocket?.sendText("[REQUEST_SYNC]")

        isConnected = true

        if (useNativeStt) {
            restartNativeListening()
        } else {
            audioEngine.startRecording()
        }
        statusText = "Listening..."
        updateBackgroundService()
        mediaSession?.isActive = true
        updateMediaSessionState()
        updateWakeLocks()
    }

    private var retryCount = 0
    private fun attemptReconnect() {
        lifecycleScope.launch(Dispatchers.Main) {
            val delayMs = minOf(1000L * (1 shl retryCount), 5000L)
            statusText = "Disconnected. Retrying in ${delayMs / 1000}s..."
            
            kotlinx.coroutines.delay(delayMs)
            
            if (isConnected) {
                retryCount++
                connectWebSocket()
            }
        }
    }

    private fun handleIncomingText(text: String) {
        try {
            when {
                text.startsWith("[HISTORY]") -> {
                    val payload = JSONObject(text.removePrefix("[HISTORY]"))
                    val archiveArr = payload.optJSONArray("archive")
                    val historyArr = payload.optJSONArray("history")
                    val parsedMessages = mutableListOf<UiMessage>()
                    
                    if (archiveArr != null) {
                        for (i in 0 until archiveArr.length()) {
                            val msg = archiveArr.getJSONObject(i)
                            parsedMessages.add(UiMessage(msg.getString("role"), msg.getString("content")))
                        }
                    }
                    
                    if (historyArr != null) {
                        for (i in 0 until historyArr.length()) {
                            val msg = historyArr.getJSONObject(i)
                            parsedMessages.add(UiMessage(msg.getString("role"), msg.getString("content")))
                        }
                    }
                    runOnUiThread {
                        messages.clear()
                        messages.addAll(parsedMessages)
                    }
                }
                text.startsWith("[SETTINGS_SYNC]") -> {
                    val s = JSONObject(text.removePrefix("[SETTINGS_SYNC]"))
                    if (s.has("provider")) {
                        val p = s.optString("provider", "ollama")
                        val un = s.optString("userName", "")
                        val gn = s.optString("googleName", googleName)
                        val ub = s.optString("userBio", "")
                        val key = s.optString("apiKey", geminiApiKey)
                        val mod = s.optString("model", "")
                        val v = s.optString("voice", "")
                        val tu = s.optJSONObject("tokenUsage")
                        runOnUiThread {
                            aiProvider = p
                            userName = un
                            googleName = gn
                            userBio = ub
                            geminiApiKey = key
                            aiModel = mod
                            aiVoice = v
                            tokenUsage.clear()
                            if (tu != null) {
                                val keys = tu.keys()
                                while (keys.hasNext()) {
                                    val k = keys.next()
                                    tokenUsage[k] = tu.optLong(k, 0L)
                                }
                            }
                            saveSettingsLocal()
                            fetchModels()
                            fetchVoices() 
                        }
                    }
                }
                text.startsWith("[TTS_CHUNK]") -> {
                    val chunk = sanitiseTTSText(text.removePrefix("[TTS_CHUNK]"))
                    if (useLocalTts && chunk.isNotBlank()) {
                        lifecycleScope.launch(Dispatchers.IO) {
                            piperMutex.withLock {
                                val audioBytes = piperEngine.synthesize(chunk, aiVoice)
                                if (audioBytes != null && audioBytes.isNotEmpty()) {
                                    audioEngine.playAudioChunk(audioBytes)
                                }
                            }
                        }
                    }
                }
                text.startsWith("[SUMMARY]") -> {
                    var parsedSummary: String
                    var pArchive = 0
                    var pMaxArchive = 100
                    var pEstTokens = 0
                    var pMaxTokens = 8192
                    try {
                        val s = JSONObject(text.removePrefix("[SUMMARY]"))
                        parsedSummary = s.optString("text", "No summary generated yet.")
                        pArchive = s.optInt("archiveTurns", 0)
                        pMaxArchive = s.optInt("maxArchiveTurns", 250)
                        pEstTokens = s.optInt("estTokens", 0)
                        pMaxTokens = s.optInt("maxTokens", 8192)
                    } catch (e: Exception) {
                        parsedSummary = text.removePrefix("[SUMMARY]")
                    }
                    runOnUiThread { 
                        memorySummary = parsedSummary
                        archiveTurns = pArchive
                        maxArchiveTurns = pMaxArchive
                        estTokens = pEstTokens
                        maxTokens = pMaxTokens
                    }
                }
                text.startsWith("[THREADS_SYNC]") -> {
                    val s = JSONObject(text.removePrefix("[THREADS_SYNC]"))
                    val parsedActiveId = s.optString("activeId", "default")
                    val threadsArr = s.optJSONArray("threads")
                    val parsedThreads = mutableListOf<ThreadItem>()
                    var parsedActiveName = "General Chat"
                    if (threadsArr != null) {
                        for (i in 0 until threadsArr.length()) {
                            val t = threadsArr.getJSONObject(i)
                            val id = t.optString("id")
                            val name = t.optString("name", "General Chat")
                            parsedThreads.add(ThreadItem(id, name))
                            if (id == parsedActiveId) {
                                parsedActiveName = name
                            }
                        }
                    }
                    runOnUiThread {
                        activeThreadId = parsedActiveId
                        activeThreadName = parsedActiveName
                        availableThreads.clear()
                        availableThreads.addAll(parsedThreads)
                    }
                }
                text == "[AI_START]" -> {
                    runOnUiThread {
                        isGeneratingAi = true
                        isAiPaused = false // Resync UI: The AI is speaking a new thought
                        messages.add(UiMessage("assistant", ""))
                        statusText = "Alyx is speaking..."
                    }
                }
                text == "[AI_END]" -> {
                    runOnUiThread {
                        isGeneratingAi = false
                        isAiPaused = false
                        statusText = "Listening..."
                    }
                }
                text.startsWith("[CHAT]:") -> {
                    val content = text.removePrefix("[CHAT]:").trim()
                    if (content.isNotBlank()) {
                        audioEngine.abortPlayback() // Sync execution on WebSocket thread to avoid race with incoming audio chunks
                        runOnUiThread {
                            isAiPaused = false // Resync UI
                            messages.add(UiMessage("user", content))
                            // Ensure it scrolls to bottom (handled by LaunchedEffect normally)
                        }
                    }
                }
                text == "[IGNORED]" -> {
                    runOnUiThread {
                        statusText = "Listening..."
                        isAiPaused = false // Resync UI: Whisper rejected the barge-in, hardware resumed
                        audioEngine.resumePlayback() // Whisper said it was just noise, resume the paused TTS!
                    }
                }
                text.startsWith("(") -> {
                    // Ignore other annotations like (keyboard clicking), (clapping), etc
                    runOnUiThread {
                        statusText = "Listening..."
                        isAiPaused = false // Resync UI: Whisper rejected the barge-in, hardware resumed
                        audioEngine.resumePlayback() // Whisper said it was just noise, resume the paused TTS!
                    }
                }
                text.startsWith("[") -> {
                    // Ignore other system sync messages like [SUMMARY], [SETTINGS], [THREADS] for now
                }
                else -> {
                    if (!isGeneratingAi && text.isNotBlank()) {
                        audioEngine.abortPlayback() // We successfully barged in, nuke the old audio queue
                    }
                    runOnUiThread {
                        if (isGeneratingAi) {
                            // Append token to the last AI message for typewriter effect
                            val lastMsg = messages.lastOrNull()
                            if (lastMsg != null && lastMsg.role == "assistant") {
                                messages[messages.lastIndex] = lastMsg.copy(content = lastMsg.content + text)
                            }
                        } else if (text.isNotBlank()) {
                            // It's the transcribed User text
                            isAiPaused = false // Resync UI
                            messages.add(UiMessage("user", text.trim()))
                        }
                    }
                }
            }
        } catch (e: Exception) {
            Log.e("MainActivity", "Error parsing WebSocket text", e)
        }
    }

    fun toggleMicMute() {
        isMicMuted = !isMicMuted
        Log.d("SpeaxUI", "Mic Muted State: $isMicMuted")
        
        if (useNativeStt) {
            if (isMicMuted) {
                stopNativeListening()
                statusText = "Muted"
            } else {
                restartNativeListening()
            }
        } else {
            audioEngine.isMicMuted = isMicMuted
            if (isMicMuted) audioEngine.forceEndStreaming()
        }
        
        updateBackgroundService()
        updateWakeLocks()
    }

    fun toggleAiPause() {
        isAiPaused = !isAiPaused
        if (isAiPaused) {
            speaxWebSocket?.sendText("[PAUSE]")
            audioEngine.suspendPlayback()
            isMicMuted = true
            audioEngine.isMicMuted = true
            audioEngine.forceEndStreaming()
            if (useNativeStt) stopNativeListening()
        } else {
            speaxWebSocket?.sendText("[RESUME]")
            audioEngine.resumePlayback()
            isMicMuted = false
            audioEngine.isMicMuted = false
            if (useNativeStt) restartNativeListening()
            else audioEngine.startRecording()
        }
        updateBackgroundService()
        updateMediaSessionState()
        updateWakeLocks()
    }

    fun switchThread(id: String) { speaxWebSocket?.sendText("[SWITCH_THREAD]:$id") }
    fun deleteThread(id: String) { speaxWebSocket?.sendText("[DELETE_THREAD]:$id") }
    fun newThread(name: String) { speaxWebSocket?.sendText("[NEW_THREAD]:$name") }
    fun renameThread(name: String) { speaxWebSocket?.sendText("[RENAME_THREAD]:$name") }
    fun sendTextPrompt(text: String) { speaxWebSocket?.sendText("[TEXT_PROMPT:${System.currentTimeMillis()}]:$text") }
    fun sendTypedPrompt(text: String) { speaxWebSocket?.sendText("[TYPED_PROMPT:${System.currentTimeMillis()}]:$text") }
    fun deleteMessagePair(index: Int) {
        speaxWebSocket?.sendText("[DELETE_MSG]:$index")
        // Optimistically remove from local UI instantly for a snappy feel
        if (index in messages.indices) {
            messages.removeAt(index) // Remove User prompt
            if (index < messages.size && messages[index].role == "assistant") {
                messages.removeAt(index) // Remove subsequent AI response
            }
        }
    }

    fun clearHistory() {
        speaxWebSocket?.sendText("[CLEAR_HISTORY]")
        messages.clear()
        memorySummary = "No summary generated yet."
    }

    fun rebuildSummary() {
        speaxWebSocket?.sendText("[REBUILD_SUMMARY]")
    }

    private fun disconnectWebSocket() {
        speaxWebSocket?.disconnect()
        speaxWebSocket = null
        audioEngine.abortPlayback()
        stopNativeListening()
        audioEngine.stopRecording()
        isConnected = false
        statusText = "Disconnected"
        updateBackgroundService()
        mediaSession?.isActive = false
        updateMediaSessionState()
        updateWakeLocks()

        // Release the audio focus back to normal so standard media apps don't sound muffled
        val audioManager = getSystemService(Context.AUDIO_SERVICE) as AudioManager
        
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            audioManager.clearCommunicationDevice()
        } else {
            @Suppress("DEPRECATION")
            audioManager.isBluetoothScoOn = false
            @Suppress("DEPRECATION")
            audioManager.stopBluetoothSco()
        }
        audioManager.mode = AudioManager.MODE_NORMAL

        // Release Audio Focus
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            (audioFocusRequest as? android.media.AudioFocusRequest)?.let {
                audioManager.abandonAudioFocusRequest(it)
            }
        } else {
            @Suppress("DEPRECATION")
            audioManager.abandonAudioFocus(focusChangeListener)
        }

        // Release WakeLock
        wakeLock?.let { if (it.isHeld) it.release() }
        wakeLock = null
    }
    
    fun logout() {
        disconnectWebSocket()
        getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().clear().apply()
        CookieManager.getInstance().removeAllCookies(null)
        sessionCookie = null
    }

    override fun onDestroy() {
        super.onDestroy()
	    unregisterReceiver(hardwareButtonReceiver)
        audioEngine.stopRecording()
        disconnectWebSocket()
        speechRecognizer?.destroy()
        mediaSession?.release()
    }
}

class PiperEngine(private val context: Context) {
    private val piperDir = File(context.filesDir, "piper_env")
    
    private var tts: OfflineTts? = null
    private var currentVoice: String = ""

    var isReady = false
        private set

    fun initEngine() {
        try {
            // Extract the contents of assets/piper/ to filesDir/piper_env/
            copyAssetFolder(context.assets, "piper", piperDir.absolutePath)
            isReady = true
            Log.d("PiperEngine", "Local Piper Engine initialized successfully.")
        } catch (e: Exception) {
            Log.e("PiperEngine", "Failed to initialize Piper", e)
        }
    }

    private fun copyAssetFolder(assetManager: android.content.res.AssetManager, fromAssetPath: String, toPath: String) {
        val file = File(toPath)
        val assets = assetManager.list(fromAssetPath) ?: return
        
        if (assets.isEmpty()) {
            // It's a file, copy it
            if (!file.exists() || file.length() == 0L) {
                file.parentFile?.mkdirs()
                assetManager.open(fromAssetPath).use { inputStream ->
                    file.outputStream().use { outputStream ->
                        inputStream.copyTo(outputStream)
                    }
                }
            }
        } else {
            // It's a directory, create it and recurse
            file.mkdirs()
            for (asset in assets) {
                copyAssetFolder(assetManager, "$fromAssetPath/$asset", "$toPath/$asset")
            }
        }
    }

    fun synthesize(text: String, voice: String): ByteArray? {
        if (!isReady || text.isBlank()) return null
        try {
            Log.d("SpeaxLocalTTS", "Attempting to synthesize chunk locally: '$text'")
            
            val modelName = if (voice.isNotBlank()) voice else "en_GB-alba-medium.onnx"
            val modelFile = piperDir.walkTopDown().firstOrNull { it.isFile && it.name == modelName }
            
            if (modelFile == null) {
                Log.e("SpeaxLocalTTS", "Model file not found locally: $modelName")
                return null
            }
            
            // Check alongside the model first, fallback to root if needed
            var espeakDataDir = File(modelFile.parentFile, "espeak-ng-data")
            if (!espeakDataDir.exists() || !espeakDataDir.isDirectory) {
                espeakDataDir = File(piperDir, "espeak-ng-data")
            }
            if (!espeakDataDir.exists() || !espeakDataDir.isDirectory) {
                Log.e("SpeaxLocalTTS", "CRITICAL FATAL: espeak-ng-data folder not found! C++ Engine will crash. Aborting.")
                return null
            }
            
            val phondataFile = File(espeakDataDir, "phondata")
            if (!phondataFile.exists() || phondataFile.length() == 0L) {
                Log.e("SpeaxLocalTTS", "CRITICAL FATAL: espeak-ng-data/phondata is missing or empty! Extraction failed. Aborting.")
                return null
            }

            // Initialize or swap models dynamically!
            if (tts == null || currentVoice != voice) {
                tts?.release()
                
                // Sherpa models come with their own perfect tokens.txt file!
                val tokensFile = File(modelFile.parentFile, "tokens.txt")
                
                if (!tokensFile.exists() || tokensFile.length() == 0L) {
                    Log.e("SpeaxLocalTTS", "CRITICAL FATAL: tokens.txt is missing from the model directory! Aborting.")
                    return null
                }
                
                Log.d("SpeaxLocalTTS", "JNI INIT: Model=${modelFile.absolutePath} (Size: ${modelFile.length()} bytes)")
                Log.d("SpeaxLocalTTS", "JNI INIT: Tokens=${tokensFile.absolutePath} (Size: ${tokensFile.length()} bytes)")
                Log.d("SpeaxLocalTTS", "JNI INIT: DataDir=${espeakDataDir.absolutePath} (phondata Size: ${phondataFile.length()} bytes)")

                val config = OfflineTtsConfig(
                    model = OfflineTtsModelConfig(
                        vits = OfflineTtsVitsModelConfig(
                            model = modelFile.absolutePath,
                            lexicon = "", // Not needed for espeak Piper models
                            tokens = if (tokensFile.exists()) tokensFile.absolutePath else "",
                            dataDir = espeakDataDir.parentFile?.absolutePath ?: piperDir.absolutePath, 
                            noiseScale = 0.667f,
                            noiseScaleW = 0.8f,
                            lengthScale = 1.0f
                        ),
                        numThreads = 1,
                        debug = true, // Force C++ Engine to print to Logcat!
                        provider = "cpu"
                    )
                )
                Log.d("SpeaxLocalTTS", "JNI INIT: Calling OfflineTts constructor...")
                tts = OfflineTts(config = config)
                Log.d("SpeaxLocalTTS", "JNI INIT: Constructor successful!")
                currentVoice = voice
            }
            
            Log.d("SpeaxLocalTTS", "JNI GENERATE: Sending text to C++ engine...")
            val audio = tts?.generate(text) ?: return null
            Log.d("SpeaxLocalTTS", "JNI GENERATE: C++ engine returned audio successfully!")

            val samples = audio.samples
            val pcmBytes = ByteArray(samples.size * 2)
            for (i in samples.indices) {
                var sample = (samples[i] * 32767.0f).toInt()
                sample = sample.coerceIn(-32768, 32767)
                pcmBytes[i * 2] = (sample and 0xFF).toByte()
                pcmBytes[i * 2 + 1] = ((sample shr 8) and 0xFF).toByte()
            }
            
            Log.d("SpeaxLocalTTS", "Success! Generated ${pcmBytes.size} bytes of audio.")
            return pcmBytes
        } catch (e: Exception) {
            Log.e("SpeaxLocalTTS", "Synthesis crashed unexpectedly: ${e.message}", e)
            return null
        }
    }
}

private val DarkColors = darkColorScheme(
    background = Color(0xFF030B17),      // Slate Deep (Depressed/Shadows)
    surface = Color(0xFF0B1E36),         // Slate Mid (Cards/Surfaces)
    surfaceVariant = Color(0xFF152E4D),  // Slate High (Active Elements/Borders)
    primary = Color(0xFF0E639C),
    secondary = Color(0xFF00D1C1),
    onBackground = Color.White,
    onSurface = Color.White,
    outline = Color(0xFF152E4D)
)

private val LightColors = lightColorScheme(
    background = Color(0xFFF5F5F5),
    surface = Color(0xFFFFFFFF),
    surfaceVariant = Color(0xFFE0E0E0),
    primary = Color(0xFF0E639C),
    secondary = Color(0xFF007A5E), // Darker green for contrast on light mode
    onBackground = Color(0xFF1E1E1E),
    onSurface = Color(0xFF1E1E1E),
    outline = Color.LightGray
)

@Composable
fun SpeaxTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = if (darkTheme) DarkColors else LightColors,
        content = content
    )
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
                                        mainActivity.micProfile = value
                                        mainActivity.getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().putString("mic_profile", value).apply()
                                        mainActivity.audioEngine.micProfile = value
                                        expandedMicDropdown = false
                                        // If using heavy, sensitive, or standard, we need to restart the engine to apply new AudioSource/FX
                                        if (value == "heavy" || mainActivity.micProfile == "heavy" || 
                                            value == "sensitive" || mainActivity.micProfile == "sensitive" ||
                                            value == "standard" || mainActivity.micProfile == "standard") {
                                            if (!mainActivity.isMicMuted) {
                                                mainActivity.audioEngine.stopRecording()
                                                mainActivity.audioEngine.startRecording()
                                            }
                                        }
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
                            Text("Only responds when addressed by name (e.g. Alyx)", color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.6f), fontSize = 12.sp)
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
                            value = if (mainActivity.isLoadingVoices) "Loading..." else if (mainActivity.aiVoice.isBlank()) "Select Voice" else mainActivity.aiVoice,
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
                                        text = { Text(selectionOption) },
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

