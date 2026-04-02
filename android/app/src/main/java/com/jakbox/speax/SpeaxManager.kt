package com.jakbox.speax

import android.content.Context
import android.content.Intent
import android.os.Build
import android.media.session.MediaSession
import android.util.Log
import androidx.compose.runtime.*
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import java.io.File
import org.json.JSONObject
import com.k2fsa.sherpa.onnx.*

data class UiMessage(val role: String, val content: String)
data class ThreadItem(val id: String, val name: String, val createdAt: String = "", val updatedAt: String = "")
data class PersonaVoice(val id: String, val name: String, val voiceFile: String)


data class SpeaxThemeData(
    val primary: String,
    val secondary: String,
    val tertiary: String,
    val background: String,
    val panel: String
)

/**
 * Central manager for Speax state, audio, and WebSocket connection.
 * Used by both MainActivity and AlyxVoiceInteractionSession.
 */
object SpeaxManager {
    private var isInitialized = false
    private lateinit var context: Context
    
    var speaxWebSocket: SpeaxWebSocket? = null
    lateinit var audioEngine: AudioEngine
    lateinit var piperEngine: PiperEngine
    lateinit var piperDir: File
    val piperMutex = Mutex()
    
    // Observable states for UI
    var isConnected by mutableStateOf(false)
    private var _isAssistantMode by mutableStateOf(false)
    var isAssistantMode: Boolean
        get() = _isAssistantMode
        set(value) {
            if (_isAssistantMode != value) {
                _isAssistantMode = value
                speaxWebSocket?.sendText("[SET_ASSISTANT:$value]")
            }
        }



    var statusText by mutableStateOf("Disconnected")
    var currentRms by mutableStateOf(0f)
    var aiRms by mutableStateOf(0f)
    var currentTtsSampleRate = 22050
    var playbackProgress by mutableStateOf(0f)
    var isMicMuted by mutableStateOf(false)
    var isAiPaused by mutableStateOf(false)
    
    private val notificationSounds = NotificationSounds()
    private var lastToneTime = 0L
    private var isIntentionallyDisconnected = true
    
    val messages = mutableStateListOf<UiMessage>()
    var sessionCookie: String? = null
    
    var aiProvider by mutableStateOf("ollama")
    var geminiApiKey by mutableStateOf("")
    var aiModel by mutableStateOf("")
    var aiVoice by mutableStateOf("")
    var userName by mutableStateOf("")
    var userBio by mutableStateOf("")
    var googleName by mutableStateOf("")
    var useLocalTts by mutableStateOf(false)
    var passiveAssistant by mutableStateOf(false)
    var useNativeStt by mutableStateOf(false)
    var isNativeSttSupported by mutableStateOf(false)
    var currentTheme by mutableStateOf<SpeaxThemeData?>(null)

    var availableModels = mutableStateListOf<String>()
    var isLoadingModels by mutableStateOf(false)
    var availableVoices = mutableStateListOf<PersonaVoice>()
    var isLoadingVoices by mutableStateOf(false)
    var micProfile by mutableStateOf("standard")
    
    val assistantName by derivedStateOf {
        availableVoices.find { it.id == aiVoice }?.name ?: "Alyx"
    }
    
    // Memory & Thread State
    var memorySummary by mutableStateOf("No summary generated yet.")
    var activeThreadId by mutableStateOf("default")
    var activeThreadName by mutableStateOf("General Chat")
    var archiveTurns by mutableStateOf(0)
    var maxArchiveTurns by mutableStateOf(100)
    var estTokens by mutableStateOf(0)
    var maxTokens by mutableStateOf(8192)
    val tokenUsage = mutableStateMapOf<String, Long>()
    val availableThreads = mutableStateListOf<ThreadItem>()
    var threadSortMode by mutableStateOf("timestamp") // "timestamp" or "alphabetical"
    var selectedThreadTab by mutableStateOf(0) // 0: General, 1: Assistant


    
    var isGeneratingAi by mutableStateOf(false)

    private val httpClient = okhttp3.OkHttpClient()

    fun fetchModels() {
        if (aiProvider == "gemini" && geminiApiKey.isBlank()) {
            availableModels.clear()
            return
        }
        isLoadingModels = true
        val url = "https://speax.jakbox.dev/api/models?provider=$aiProvider&apiKey=$geminiApiKey"
        
        CoroutineScope(Dispatchers.IO).launch {
            try {
                val request = okhttp3.Request.Builder().url(url).build()
                val response = httpClient.newCall(request).execute()
                if (response.isSuccessful) {
                    val jsonStr = response.body?.string() ?: "[]"
                    val jsonArray = org.json.JSONArray(jsonStr)
                    val models = mutableListOf<String>()
                    for (i in 0 until jsonArray.length()) {
                        models.add(jsonArray.getJSONObject(i).getString("id"))
                    }
                    launch(Dispatchers.Main) {
                        availableModels.clear()
                        availableModels.addAll(models)
                    }
                }
            } catch (e: Exception) { Log.e("SpeaxManager", "Error fetching models", e) }
            finally { launch(Dispatchers.Main) { isLoadingModels = false } }
        }
    }
    
    fun fetchVoices() {
        isLoadingVoices = true
        CoroutineScope(Dispatchers.IO).launch {
            try {
                val personasList = mutableListOf<PersonaVoice>()
                val url = "https://speax.jakbox.dev/api/voices"
                val request = okhttp3.Request.Builder().url(url).build()
                val response = httpClient.newCall(request).execute()
                
                if (response.isSuccessful) {
                    val jsonStr = response.body?.string() ?: "[]"
                    val jsonArray = org.json.JSONArray(jsonStr)
                    for (i in 0 until jsonArray.length()) {
                        val obj = jsonArray.getJSONObject(i)
                        personasList.add(PersonaVoice(
                            id = obj.getString("id"),
                            name = obj.getString("name"),
                            voiceFile = obj.getString("voice_file")
                        ))
                    }
                }

                val finalVoices = if (useLocalTts) {
                    // Filter by what we have locally
                    val modelsDir = File(piperDir, "models")
                    personasList.filter { p ->
                        File(modelsDir, p.voiceFile).exists() || 
                        piperDir.walkTopDown().any { it.isFile && it.name == p.voiceFile }
                    }
                } else {
                    personasList
                }

                launch(Dispatchers.Main) {
                    availableVoices.clear()
                    availableVoices.addAll(finalVoices)
                }
            } catch (e: Exception) { Log.e("SpeaxManager", "Error fetching voices", e) }
            finally { launch(Dispatchers.Main) { isLoadingVoices = false } }
        }
    }

    fun init(context: Context) {
        if (isInitialized) return
        this.context = context.applicationContext
        isInitialized = true
        piperDir = File(this.context.filesDir, "piper_env")
        
        // Initialize Audio Engine
        audioEngine = AudioEngine(
            onSpeechFinalized = { _, _ -> },
            onVolumeChange = { rms ->
                currentRms = rms
            },
            onSpeechStart = {
                statusText = "Recording (Speaking)..."
            },
            onAiVolumeChange = { rms ->
                aiRms = rms
            },
            onBufferProgress = { progress ->
                playbackProgress = progress
            },
            onPlaybackComplete = {
                speaxWebSocket?.sendText("[PLAYBACK_COMPLETE]")
                audioEngine.isPlaybackActive = false
                restoreMicMuteState()
            },
            onStreamingChunk = { pcmData, seqID, type ->
                speaxWebSocket?.sendStreamingChunk(type, seqID, pcmData)
                if (type == 0x02.toByte()) {
                    statusText = "Processing with AI..."
                }
            }
        )
        
        val prefs = context.getSharedPreferences("speax_prefs", Context.MODE_PRIVATE)
        sessionCookie = prefs.getString("session_cookie", null)
        aiProvider = prefs.getString("provider", "ollama") ?: "ollama"
        geminiApiKey = prefs.getString("api_key", "") ?: ""
        aiModel = prefs.getString("model", "") ?: ""
        aiVoice = prefs.getString("voice", "") ?: ""
        userName = prefs.getString("user_name", "") ?: ""
        userBio = prefs.getString("user_bio", "") ?: ""
        googleName = prefs.getString("google_name", "") ?: ""
        useLocalTts = prefs.getBoolean("use_local_tts", false)
        passiveAssistant = prefs.getBoolean("passive_assistant", false)
        useNativeStt = prefs.getBoolean("use_native_stt", false)
        isNativeSttSupported = android.speech.SpeechRecognizer.isRecognitionAvailable(this.context)
        micProfile = prefs.getString("mic_profile", "standard") ?: "standard"
        audioEngine.micProfile = micProfile
        
        // Initialize Piper Engine
        piperEngine = PiperEngine(this.context)
        CoroutineScope(Dispatchers.IO).launch {
            piperEngine.initEngine()
        }
    }

    fun pushSettingsToServer() {
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

    fun saveSettingsLocal() {
        context.getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().apply {
            putString("provider", aiProvider)
            putString("api_key", geminiApiKey)
            putString("model", aiModel)
            putString("voice", aiVoice)
            putString("user_name", userName)
            putString("user_bio", userBio)
            putString("google_name", googleName)
            putBoolean("use_local_tts", useLocalTts)
            putBoolean("passive_assistant", passiveAssistant)
            putBoolean("use_native_stt", useNativeStt)
        }.apply()
    }
    
    private var cpuWakeLock: android.os.PowerManager.WakeLock? = null

    fun connect(sessionToken: MediaSession.Token? = null) {
        if (sessionCookie == null) return
        isIntentionallyDisconnected = false
        
        // 1. Audio Reset (in case we're stuck in a previous state)
        teardownAudioSystem()
        
        // 2. Audio Setup
        val audioManager = context.getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
        audioManager.mode = android.media.AudioManager.MODE_IN_COMMUNICATION
        
        // 3. Bluetooth routing
        if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.S) {
            val devices = audioManager.availableCommunicationDevices
            val btDevice = devices.firstOrNull { 
                it.type == android.media.AudioDeviceInfo.TYPE_BLUETOOTH_SCO || it.type == android.media.AudioDeviceInfo.TYPE_BLE_HEADSET 
            }
            if (btDevice != null) audioManager.setCommunicationDevice(btDevice)
        } else {
            @Suppress("DEPRECATION")
            audioManager.startBluetoothSco()
            @Suppress("DEPRECATION")
            audioManager.isBluetoothScoOn = true
        }
        
        // 4. Audio Focus
        val focusChangeListener = android.media.AudioManager.OnAudioFocusChangeListener { _ -> }
        if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.O) {
            val request = android.media.AudioFocusRequest.Builder(android.media.AudioManager.AUDIOFOCUS_GAIN)
                .setAudioAttributes(
                    android.media.AudioAttributes.Builder()
                        .setUsage(android.media.AudioAttributes.USAGE_VOICE_COMMUNICATION)
                        .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build()
                )
                .setOnAudioFocusChangeListener(focusChangeListener)
                .build()
            audioManager.requestAudioFocus(request)
        } else {
            @Suppress("DEPRECATION")
            audioManager.requestAudioFocus(focusChangeListener, android.media.AudioManager.STREAM_VOICE_CALL, android.media.AudioManager.AUDIOFOCUS_GAIN)
        }
 
        // 5. WakeLock
        if (cpuWakeLock == null) {
            val pm = context.getSystemService(Context.POWER_SERVICE) as android.os.PowerManager
            cpuWakeLock = pm.newWakeLock(android.os.PowerManager.PARTIAL_WAKE_LOCK, "speax:mic_active")
            cpuWakeLock?.acquire()
        }

        // 5. Foreground Service (CRITICAL for Mic access in background)
        val serviceIntent = Intent(context, SpeaxService::class.java).apply {
            action = "START"
            putExtra("is_muted", isMicMuted || isAiPaused)
            if (sessionToken != null) {
                putExtra("session_token", sessionToken)
            }
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            try {
                context.startForegroundService(serviceIntent)
            } catch (e: Exception) {
                Log.w("SpeaxManager", "Failed to start foreground service. Proceeding without it (VoiceInteraction usually handles mic permissions). Error: ${e.message}")
            }
        } else {
            try {
                context.startService(serviceIntent)
            } catch (e: Exception) {
                Log.w("SpeaxManager", "Failed to start service: ${e.message}")
            }
        }

        val serverUrl = "wss://speax.jakbox.dev/ws"
        val deviceName = java.net.URLEncoder.encode("${android.os.Build.MANUFACTURER} ${android.os.Build.MODEL}", "UTF-8")
        val fullUrl = "$serverUrl?client=android&device=$deviceName&version=1"
        
        statusText = "Connecting..."
        speaxWebSocket?.disconnect()
        
        speaxWebSocket = SpeaxWebSocket(
            url = fullUrl,
            cookie = sessionCookie!!,
            onTextReceived = { ws, text ->
                if (ws === speaxWebSocket) handleIncomingText(text)
            },
            onAudioReceived = { ws, audioBytes ->
                if (ws === speaxWebSocket) audioEngine.playAudioChunk(audioBytes, currentTtsSampleRate)
            },
            onConnected = { ws ->
                if (ws === speaxWebSocket) {
                    isConnected = true
                    statusText = "Connected"
                    ws.sendText("[REQUEST_SYNC]")
                    
                    val now = System.currentTimeMillis()
                    if (now - lastToneTime > 2000) {
                        notificationSounds.playConnect()
                        lastToneTime = now
                    }
                    
                    // Added: Give the audio system (Bluetooth/Focus/Mode) a moment to settle 
                    // before we aggressively jam the mic open!
                    CoroutineScope(Dispatchers.IO).launch {
                        kotlinx.coroutines.delay(300) 
                        syncRecordingState()
                    }
                }
            },
            onClosed = { ws ->
                if (ws === speaxWebSocket) {
                    isConnected = false
                    statusText = "Disconnected"
                    syncRecordingState()
                    
                    val now = System.currentTimeMillis()
                    if (now - lastToneTime > 2000) {
                        notificationSounds.playDisconnect()
                        lastToneTime = now
                    }
                    
                    if (!isIntentionallyDisconnected) {
                        CoroutineScope(Dispatchers.IO).launch {
                            kotlinx.coroutines.delay(2000)
                            if (!isConnected && !isIntentionallyDisconnected) {
                                Log.d("SpeaxManager", "Auto-reconnecting...")
                                connect()
                            }
                        }
                    }
                }
            }
        )
        speaxWebSocket?.connect()
    }
    
    fun disconnect() {
        isIntentionallyDisconnected = true
        speaxWebSocket?.disconnect()
        isConnected = false
        statusText = "Disconnected"
        syncRecordingState()
        
        cpuWakeLock?.let {
            if (it.isHeld) it.release()
        }
        cpuWakeLock = null

        teardownAudioSystem()

        val serviceIntent = Intent(context, SpeaxService::class.java).apply {
            action = "STOP"
        }
        context.startService(serviceIntent)
    }

    private fun teardownAudioSystem() {
        val audioManager = context.getSystemService(Context.AUDIO_SERVICE) as android.media.AudioManager
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            audioManager.clearCommunicationDevice()
        } else {
            @Suppress("DEPRECATION")
            audioManager.isBluetoothScoOn = false
            @Suppress("DEPRECATION")
            audioManager.stopBluetoothSco()
        }
        audioManager.mode = android.media.AudioManager.MODE_NORMAL
    }
    
    fun updateMicProfile(newProfile: String) {
        val oldProfile = micProfile
        micProfile = newProfile
        audioEngine.micProfile = newProfile
        
        Log.d("SpeaxManager", "Updating Mic Profile: $oldProfile -> $newProfile")
        
        // Immediate enactment: If we are currently in an AI turn or playing back,
        // we need to apply or remove the mute immediately.
        if (isGeneratingAi || playbackProgress > 0f) {
            if (newProfile == "mute_playback") {
                Log.d("SpeaxManager", "Enacting auto-mute immediately as AI is active")
                audioEngine.isMicMuted = true
            } else if (oldProfile == "mute_playback") {
                Log.d("SpeaxManager", "Releasing auto-mute immediately as profile changed")
                audioEngine.isMicMuted = isMicMuted // Restore to manual user state
            }
        }
        
        // Persist
        context.getSharedPreferences("speax_prefs", Context.MODE_PRIVATE).edit().putString("mic_profile", newProfile).apply()
    }
    
    fun toggleMicMute() {
        if (!isMicMuted && audioEngine.isRecording) {
            speaxWebSocket?.sendText("[CANCEL]")
        }
        isMicMuted = !isMicMuted
        audioEngine.isMicMuted = isMicMuted
        Log.d("SpeaxManager", "toggleMicMute: isMicMuted=$isMicMuted")
        syncRecordingState()
    }
    
    fun toggleAiPause() {
        isAiPaused = !isAiPaused
        Log.d("SpeaxManager", "toggleAiPause: isAiPaused=$isAiPaused")
        if (isAiPaused) {
            speaxWebSocket?.sendText("[PAUSE]")
            audioEngine.suspendPlayback()
        } else {
            speaxWebSocket?.sendText("[RESUME]")
            audioEngine.resumePlayback()
        }
        syncRecordingState()
    }

    /**
     * Centralized logic to ensure the microphone hardware state matches the app's logical state.
     */
    fun syncRecordingState() {
        if (isConnected && !isMicMuted && !isAiPaused && !useNativeStt) {
            Log.d("SpeaxManager", "syncRecordingState: All conditions met, starting mic.")
            audioEngine.startRecording()
        } else {
            Log.d("SpeaxManager", "syncRecordingState: Mic should be off (connected=$isConnected, muted=$isMicMuted, paused=$isAiPaused, nativeStt=$useNativeStt). Stopping.")
            audioEngine.stopRecording()
        }
    }
    
    private fun handleIncomingText(text: String) {
        try {
            when {
                text.startsWith("[STATUS]") -> {
                    statusText = text.removePrefix("[STATUS]")
                }
                text.startsWith("[HISTORY]") -> {
                    val payload = JSONObject(text.removePrefix("[HISTORY]"))
                    val archiveArr = payload.optJSONArray("archive")
                    val historyArr = payload.optJSONArray("history")
                    val parsedMessages = mutableListOf<UiMessage>()
                    
                    archiveArr?.let {
                        for (i in 0 until it.length()) {
                            val msg = it.getJSONObject(i)
                            parsedMessages.add(UiMessage(msg.getString("role"), msg.getString("content")))
                        }
                    }
                    historyArr?.let {
                        for (i in 0 until it.length()) {
                            val msg = it.getJSONObject(i)
                            parsedMessages.add(UiMessage(msg.getString("role"), msg.getString("content")))
                        }
                    }
                    CoroutineScope(Dispatchers.Main).launch {
                        messages.clear()
                        messages.addAll(parsedMessages)
                    }
                }
                text.startsWith("[SETTINGS_SYNC]") -> {
                    val s = JSONObject(text.removePrefix("[SETTINGS_SYNC]"))
                    CoroutineScope(Dispatchers.Main).launch {
                        aiProvider = s.optString("provider", aiProvider)
                        userName = s.optString("userName", userName)
                        googleName = s.optString("googleName", googleName)
                        userBio = s.optString("userBio", userBio)
                        geminiApiKey = s.optString("apiKey", geminiApiKey)
                        aiModel = s.optString("model", aiModel)
                        aiVoice = s.optString("voice", aiVoice)
                        useLocalTts = s.optBoolean("clientTts", useLocalTts)
                        passiveAssistant = s.optBoolean("passiveAssistant", passiveAssistant)
                        
                        val tu = s.optJSONObject("tokenUsage")
                        if (tu != null) {
                            tokenUsage.clear()
                            val keys = tu.keys()
                            while (keys.hasNext()) {
                                val k = keys.next()
                                tokenUsage[k] = tu.optLong(k, 0L)
                            }
                        }
                        val themeObj = s.optJSONObject("theme")
                        if (themeObj != null) {
                            currentTheme = SpeaxThemeData(
                                primary = themeObj.optString("primary", "#0E639C"),
                                secondary = themeObj.optString("secondary", "#00D1C1"),
                                tertiary = themeObj.optString("tertiary", "#005A53"),
                                background = themeObj.optString("background", "#051329"),
                                panel = themeObj.optString("panel", "#0B1E36")
                            )
                        } else {
                            currentTheme = null
                        }

                        saveSettingsLocal()
                        fetchModels()
                        fetchVoices()
                    }
                }
                text.startsWith("[TTS_CHUNK]") -> {
                    val content = text.removePrefix("[TTS_CHUNK]")
                    if (content.startsWith("{")) {
                        try {
                            val json = JSONObject(content)
                            when (json.optString("type")) {
                                "start" -> {
                                    currentTtsSampleRate = json.optInt("sampleRate", 22050)
                                    audioEngine.prepareForNewStream()
                                    Log.d("SpeaxManager", "Remote TTS Start: sampleRate=$currentTtsSampleRate")
                                }
                            }
                        } catch (e: Exception) {
                            Log.e("SpeaxManager", "Failed to parse TTS_CHUNK JSON: $content", e)
                        }
                    } else {
                        val chunk = sanitiseTTSText(content)
                        if (useLocalTts && chunk.isNotBlank()) {
                            CoroutineScope(Dispatchers.IO).launch {
                                piperMutex.withLock {
                                    val persona = availableVoices.find { it.id == aiVoice }
                                    val voiceToUse = persona?.voiceFile ?: aiVoice
                                    val audioBytes = piperEngine.synthesize(chunk, voiceToUse)
                                    if (audioBytes != null && audioBytes.isNotEmpty()) {
                                        audioEngine.playAudioChunk(audioBytes)
                                    }
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
                    CoroutineScope(Dispatchers.Main).launch {
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
                            val createdAt = t.optString("createdAt", "")
                            val updatedAt = t.optString("updatedAt", "")
                            parsedThreads.add(ThreadItem(id, name, createdAt, updatedAt))
                            if (id == parsedActiveId) {
                                parsedActiveName = name
                            }
                        }
                    }
                    // Sort assistant threads newest to oldest by default (or current mode)
                    sortThreadsInternal(parsedThreads)


                    CoroutineScope(Dispatchers.Main).launch {
                        activeThreadId = parsedActiveId
                        activeThreadName = parsedActiveName
                        availableThreads.clear()
                        availableThreads.addAll(parsedThreads)
                    }
                }
                text == "[AI_START]" -> {
                    CoroutineScope(Dispatchers.Main).launch {
                        isGeneratingAi = true
                        isAiPaused = false 
                        messages.add(UiMessage("assistant", ""))
                        statusText = "$assistantName is speaking..."
                        
                        if (micProfile == "mute_playback") {
                            Log.d("SpeaxManager", "AI_START: Auto-muting mic for playback")
                            audioEngine.isMicMuted = true
                        }
                        audioEngine.isPlaybackActive = true
                    }
                }
                text == "[AI_END]" -> {
                    CoroutineScope(Dispatchers.Main).launch {
                        isGeneratingAi = false
                        isAiPaused = false
                        statusText = "Listening..."
                        
                        if (micProfile == "mute_playback") {
                            Log.d("SpeaxManager", "AI_END: Generation finished. Mic will unmute when playback completes.")
                            // We do NOT restore here anymore, we wait for onPlaybackComplete!
                        }
                    }
                }
                text.startsWith("[WHISPER_STATUS]") -> {
                    // Status updates are handled globally, we just consume the message here
                    // to prevent it from falling into the 'else' block and being added to history.
                    Log.d("SpeaxManager", "Whisper status update received")
                }
                text.startsWith("[CHAT]:") -> {
                    val content = text.removePrefix("[CHAT]:").trim()
                    if (content.isNotBlank()) {
                        audioEngine.abortPlayback()
                        CoroutineScope(Dispatchers.Main).launch {
                            isAiPaused = false
                            messages.add(UiMessage("user", content))
                            audioEngine.isPlaybackActive = false
                            restoreMicMuteState()
                        }
                    }
                }
                text == "[IGNORED]" -> {
                    CoroutineScope(Dispatchers.Main).launch {
                        statusText = "Listening..."
                        isAiPaused = false
                        audioEngine.isPlaybackActive = false
                        audioEngine.resumePlayback()
                        restoreMicMuteState()
                    }
                }
                text.startsWith("(") -> {
                    CoroutineScope(Dispatchers.Main).launch {
                        statusText = "Listening..."
                        isAiPaused = false
                        audioEngine.isPlaybackActive = false
                        audioEngine.resumePlayback()
                        restoreMicMuteState()
                    }
                }
                else -> {
                    if (!isGeneratingAi && text.isNotBlank()) {
                        audioEngine.abortPlayback()
                        audioEngine.isPlaybackActive = false
                        restoreMicMuteState()
                    }
                    CoroutineScope(Dispatchers.Main).launch {
                        if (isGeneratingAi) {
                            val lastMsg = messages.lastOrNull()
                            if (lastMsg != null && lastMsg.role == "assistant") {
                                messages[messages.lastIndex] = lastMsg.copy(content = lastMsg.content + text)
                            }
                        } else if (text.isNotBlank()) {
                            isAiPaused = false
                            messages.add(UiMessage("user", text.trim()))
                        }
                    }
                }
            }
        } catch (e: Exception) {
            Log.e("SpeaxManager", "Error parsing text: $text", e)
        }
        Log.d("SpeaxManager", "Received: $text")
    }
    
    fun switchThread(id: String) { speaxWebSocket?.sendText("[SWITCH_THREAD]:$id") }
    fun deleteThread(id: String) { speaxWebSocket?.sendText("[DELETE_THREAD]:$id") }
    fun newThread(name: String) { speaxWebSocket?.sendText("[NEW_THREAD]:$name") }
    fun renameThread(name: String) { speaxWebSocket?.sendText("[RENAME_THREAD]:$name") }
    
    fun sendTextPrompt(text: String) { 
        val tag = if (isAssistantMode) "TEXT_PROMPT:${System.currentTimeMillis()}:ASSISTANT" else "TEXT_PROMPT:${System.currentTimeMillis()}"
        speaxWebSocket?.sendText("[$tag]:$text") 
    }
    
    fun sendTypedPrompt(text: String) { 
        val tag = if (isAssistantMode) "TYPED_PROMPT:${System.currentTimeMillis()}:ASSISTANT" else "TYPED_PROMPT:${System.currentTimeMillis()}"
        speaxWebSocket?.sendText("[$tag]:$text") 
    }

    
    fun deleteMessage(index: Int) {
        speaxWebSocket?.sendText("[DELETE_MSG]:$index")
    }

    fun deleteMessagePair(index: Int) {
        // AI response is always index + 1 if the role is user
        if (index < messages.size && messages[index].role == "user") {
            deleteMessage(index)
        }
    }

    fun clearHistory() {
        speaxWebSocket?.sendText("[CLEAR_HISTORY]")
    }

    fun rebuildSummary() {
        speaxWebSocket?.sendText("[REBUILD_SUMMARY]")
    }

    fun sortThreadsInternal(list: MutableList<ThreadItem>) {
        if (threadSortMode == "alphabetical") {
            list.sortBy { it.name.lowercase() }
        } else {
            // Sort by updatedAt descending (fallback to createdAt if missing)
            list.sortWith(compareByDescending<ThreadItem> { 
                if (it.updatedAt.isNotBlank()) it.updatedAt else it.createdAt 
            }.thenByDescending { it.id })
        }
    }

    fun resortThreads() {
        val current = availableThreads.toMutableList()
        sortThreadsInternal(current)
        availableThreads.clear()
        availableThreads.addAll(current)
    }


    fun cleanVoiceName(name: String): String {
        return name.replace(".onnx", "")
                   .replace(Regex("-(qint8|int8|fp16|low|medium|high|standard)$"), "")
    }

    private fun restoreMicMuteState() {
        if (micProfile == "mute_playback") {
            Log.d("SpeaxManager", "restoring mic to manual state: $isMicMuted")
            audioEngine.isMicMuted = isMicMuted
        }
    }

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
}

class PiperEngine(private val context: Context) {
    private val piperDir get() = SpeaxManager.piperDir
    
    private var tts: com.k2fsa.sherpa.onnx.OfflineTts? = null
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
            var assetSize = 0L
            try {
                assetManager.openFd(fromAssetPath).use { fd ->
                    assetSize = fd.length
                }
            } catch (e: Exception) {
                // openFd might fail for compressed assets
            }

            if (!file.exists() || file.length() == 0L || (assetSize > 0 && file.length() != assetSize)) {
                Log.d("PiperEngine", "Extracting asset: $fromAssetPath to ${file.absolutePath} (Size: $assetSize)")
                file.parentFile?.mkdirs()
                assetManager.open(fromAssetPath).use { inputStream ->
                    file.outputStream().use { outputStream ->
                        inputStream.copyTo(outputStream)
                    }
                }
            }
        } else {
            // It's a directory, recursion
            if (!file.exists()) file.mkdirs()
            for (asset in assets) {
                copyAssetFolder(assetManager, "$fromAssetPath/$asset", "$toPath/$asset")
            }
        }
    }

    fun synthesize(text: String, voice: String): ByteArray? {
        if (!isReady || text.isBlank()) return null
        try {
            Log.d("SpeaxLocalTTS", "Attempting to synthesize chunk locally: '$text'")
            
            val modelName = if (voice.isNotBlank()) {
                if (voice.endsWith(".onnx")) voice else "$voice.onnx"
            } else "alyx-qint8.onnx"
            
            // Prefer models in the 'models' directory first
            var modelFile = File(piperDir, "models/$modelName")
            if (!modelFile.exists()) {
                // Fallback to searching everywhere in piperDir (legacy assets support)
                modelFile = piperDir.walkTopDown().firstOrNull { it.isFile && it.name == modelName } ?: modelFile
            }
            
            if (!modelFile.exists()) {
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
                // Prioritize model-specific tokens files (like model.onnx.tokens.txt or model.tokens.txt)
                val modelNameNoExt = modelFile.name.replace(".onnx", "")
                val modelSpecificTokensNames = listOf("${modelFile.name}.tokens.txt", "$modelNameNoExt.tokens.txt")
                
                var tokensFile = File(modelFile.parentFile, "tokens.txt")
                for (name in modelSpecificTokensNames) {
                    val specific = File(modelFile.parentFile, name)
                    if (specific.exists() && specific.length() > 0L) {
                        tokensFile = specific
                        break
                    }
                }

                if (!tokensFile.exists() || tokensFile.length() == 0L) {
                    tokensFile = File(piperDir, "tokens.txt")
                }
                
                // If still not found, search the whole environment (handles subdirs like vits-piper-...)
                if (!tokensFile.exists() || tokensFile.length() == 0L) {
                    tokensFile = piperDir.walkTopDown().firstOrNull { it.isFile && (it.name == "tokens.txt" || it.name in modelSpecificTokensNames) } ?: tokensFile
                }

                if (!tokensFile.exists() || tokensFile.length() == 0L) {
                    Log.e("SpeaxLocalTTS", "CRITICAL FATAL: tokens.txt (or model-specific tokens) is missing! Aborting.")
                    return null
                }
                
                Log.d("SpeaxLocalTTS", "JNI INIT: Model=${modelFile.absolutePath} (Size: ${modelFile.length()} bytes)")
                Log.d("SpeaxLocalTTS", "JNI INIT: Tokens=${tokensFile.absolutePath} (Size: ${tokensFile.length()} bytes)")
                Log.d("SpeaxLocalTTS", "JNI INIT: DataDir=${espeakDataDir.absolutePath} (phondata Size: ${phondataFile.length()} bytes)")

                val vitsConfig = com.k2fsa.sherpa.onnx.OfflineTtsVitsModelConfig(
                    model = modelFile.absolutePath,
                    lexicon = "", // Not needed for espeak Piper models
                    tokens = if (tokensFile.exists()) tokensFile.absolutePath else "",
                    dataDir = espeakDataDir.absolutePath, 
                    noiseScale = 0.667f,
                    noiseScaleW = 0.8f,
                    lengthScale = 1.0f
                )

                val modelConfig = com.k2fsa.sherpa.onnx.OfflineTtsModelConfig(
                    vits = vitsConfig,
                    numThreads = 1,
                    debug = true, // Force C++ Engine to print to Logcat!
                    provider = "cpu"
                )

                val config = com.k2fsa.sherpa.onnx.OfflineTtsConfig(
                    model = modelConfig,
                    ruleFsts = "",
                    maxNumSentences = 1
                )
                
                Log.d("SpeaxLocalTTS", "JNI CONFIG: model=${vitsConfig.model}")
                Log.d("SpeaxLocalTTS", "JNI CONFIG: tokens=${vitsConfig.tokens}")
                Log.d("SpeaxLocalTTS", "JNI CONFIG: dataDir=${vitsConfig.dataDir}")
                Log.d("SpeaxLocalTTS", "JNI CONFIG: numThreads=${modelConfig.numThreads}")
                Log.d("SpeaxLocalTTS", "JNI CONFIG: provider=${modelConfig.provider}")

                Log.d("SpeaxLocalTTS", "JNI INIT: Calling OfflineTts constructor...")
                tts = com.k2fsa.sherpa.onnx.OfflineTts(config = config)
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
        } catch (e: Throwable) {
            Log.e("SpeaxLocalTTS", "CRITICAL: Synthesis/Init failed!", e)
            e.printStackTrace()
            return null
        }
    }
}
