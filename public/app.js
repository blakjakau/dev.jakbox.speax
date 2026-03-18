import { MemoryManager } from './memory.js';
import { ServerClient } from './server.js';

let outContext;
let inContext;
let processor;
let input;

let audioChunks = [];
let audioQueue = [];
let isPlayingAudio = false;
let currentAiDiv = null;
let currentAudioSource = null;
let audioAnalyser = null;
let inputAnalyser = null;
let inputVisualizerId = null;
let visualizerId = null;
let preRollBuffer = [];
let isSpeaking = false;
let silenceFrames = 0;
let wakeLock = null;
let isMuted = false;
let reconnectDelay = 1000;
const MAX_RECONNECT_DELAY = 5000;
let isDictationActive = false;
let isPaused = false;
let speechStartTime = 0; // Recorded when VAD trips
let lastSummaryData = { estTokens: 0, maxTokens: 8192, archiveTurns: 0, maxArchiveTurns: 250, text: "No summary generated yet." };
let lastTokenUsage = {};

// Web STT Engine States
let recognition = null;
let useNativeStt = false;
let isRecognitionActive = false;
let statusResetTimeout = null;
let nativeSttBuffer = "";
let nativeSttDebounceTimer = null;
const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
const isNativeSttSupported = !!SpeechRecognition;

const NOISE_THRESHOLD = 0.015; // RMS threshold. Increase if your room is noisy!
let averageSpeechRms = 0.05; // Baseline adaptive RMS tracker
const SILENCE_FRAMES_LIMIT = 6; // ~0.75 seconds of trailing silence to stop
const PRE_ROLL_FRAMES = 2; // ~0.5 seconds of audio to keep BEFORE speech is detected
const MIN_CHUNKS = 2; // Require at least ~0.5 seconds of audio to bother sending

const ICON_POWER = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><path d="M18.36 6.64a9 9 0 1 1-12.73 0"></path><line x1="12" y1="2" x2="12" y2="12"></line></svg>`;
const ICON_ALYX = `<svg viewBox="0 0 100 100" xmlns="http://www.w3.org/2000/svg" style="width: 50%; height: 50%;">
  <g fill="currentColor">
    <!-- Shadow / Thought Layer -->
    <g style="opacity: 0.35;">
      <rect x="14" y="42.65" width="8" height="8.36" rx="4" /><rect x="26" y="27.60" width="8" height="28.35" rx="4" />
      <rect x="38" y="17.33" width="8" height="54.05" rx="4" /><rect x="50" y="22.74" width="8" height="67.76" rx="4" />
      <rect x="62" y="40.14" width="8" height="45.75" rx="4" /><rect x="74" y="47.46" width="8" height="21.13" rx="4" />
      <rect x="86" y="48.02" width="8" height="6.40" rx="4" />
    </g>
    <!-- Active / Speech Layer -->
    <g>
      <rect x="10" y="45.00" width="8" height="13.37" rx="4" /><rect x="22" y="44.67" width="8" height="27.57" rx="4" />
      <rect x="34" y="45.99" width="8" height="32.26" rx="4" /><rect x="46" y="38.51" width="8" height="31.38" rx="4" />
      <rect x="58" y="29.07" width="8" height="26.99" rx="4" /><rect x="70" y="29.93" width="8" height="22.98" rx="4" />
      <rect x="82" y="41.02" width="8" height="12.13" rx="4" />
    </g>
  </g>
</svg>`;
const ICON_WAIT = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><path d="M12 2v4M12 18v4M4.93 4.93l2.83 2.83M16.24 16.24l2.83 2.83M2 12h4M18 12h4M4.93 19.07l2.83-2.83M16.24 7.76l2.83-2.83"></path></svg>`;
const ICON_MIC = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"></path><path d="M19 10v2a7 7 0 0 1-14 0v-2"></path><line x1="12" y1="19" x2="12" y2="22"></line></svg>`;
const ICON_MIC_MUTE = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><line x1="2" y1="2" x2="22" y2="22"></line><path d="M18.89 13.23A7.12 7.12 0 0 0 19 12v-2"></path><path d="M5 10v2a7 7 0 0 0 12 5"></path><path d="M15 9.34V5a3 3 0 0 0-5.68-1.33"></path><path d="M9 9v3a3 3 0 0 0 5.12 2.12"></path><line x1="12" y1="19" x2="12" y2="22"></line></svg>`;
const ICON_PAUSE = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><rect x="6" y="4" width="4" height="16"></rect><rect x="14" y="4" width="4" height="16"></rect></svg>`;
const ICON_PLAY = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="width: 50%; height: 50%;"><polygon points="5 3 19 12 5 21 5 3"></polygon></svg>`;

async function getDeviceInfo() {
    if (navigator.userAgentData && navigator.userAgentData.getHighEntropyValues) {
        try {
            const uaData = await navigator.userAgentData.getHighEntropyValues(["model", "brands"]);
            // Find the most significant browser brand, filtering out generic ones.
            const browser = uaData.brands.find(b => !/Not.?A.?Brand|Chromium/.test(b.brand)) || uaData.brands[0];
            const browserName = browser.brand;
            const model = uaData.model ? ` on ${uaData.model}` : ''; // e.g., "on Pixel 7"
            return `${browserName}${model}`;
        } catch (error) {
            console.warn("Could not get high-entropy user agent data, falling back.", error);
        }
    }

    // Fallback for browsers that don't support User-Agent Client Hints API
    const ua = navigator.userAgent;
    if (/Android/i.test(ua)) return "Android Browser";
    if (/iPhone|iPad|iPod/i.test(ua)) return "iOS Browser";
    if (/Windows/i.test(ua)) return "Windows Browser";
    if (/Macintosh/i.test(ua)) return "macOS Browser";
    if (/Linux/i.test(ua)) return "Linux Browser";
    return "Unknown Browser";
}

const status = document.getElementById('status');
const mainToggleBtn = document.getElementById('mainToggleBtn');
const transcript = document.getElementById('transcript');
const homeTabBtn = document.getElementById('tab-home');
const threadTabBtn = document.getElementById('tab-thread');
const memoryTabBtn = document.getElementById('tab-memory');
const homeView = document.getElementById('view-home');
const summaryView = document.getElementById('summary-view');
const summaryContent = document.getElementById('summary-content');
const authSection = document.getElementById('auth-section');
const appSection = document.getElementById('app-section');
const loginBtn = document.getElementById('loginBtn');
const clearHistoryBtn = document.getElementById('clearHistoryBtn');
const rebuildSummaryBtn = document.getElementById('rebuildSummaryBtn');
const muteBtn = document.getElementById('muteBtn');
const pauseBtn = document.getElementById('pauseBtn');

const progressContainer = document.getElementById('audioProgressContainer');
const progressBar = document.getElementById('audioProgressBar');
let aiAudioTotalSecs = 0;
let aiAudioPlayedSecs = 0;
let aiChunkStartContextTime = 0;
let aiChunkDuration = 0;
let progressAnimId = null;
let currentVisualPercent = 0;
let progressLogThrottler = 0;

const userProfileContainer = document.getElementById('userProfileContainer');
const userAvatarBtn = document.getElementById('userAvatarBtn');
const menuSettingsBtn = document.getElementById('menuSettingsBtn');
const menuLogoutBtn = document.getElementById('menuLogoutBtn');

const threadsFab = document.getElementById('threadsFab');
if (threadsFab) threadsFab.style.transition = 'bottom 0.2s ease-out';
const threadsSidebar = document.getElementById('threadsSidebar');
const closeThreadsBtn = document.getElementById('closeThreadsBtn');
const newThreadBtn = document.getElementById('newThreadBtn');
const threadList = document.getElementById('threadList');
const threadNameInput = document.getElementById('threadNameInput');
const drawerOverlay = document.getElementById('drawerOverlay');

const settingsSidebar = document.getElementById('settingsSidebar');
const closeSettingsBtn = document.getElementById('closeSettingsBtn');
const userName = document.getElementById('userName');
const userBio = document.getElementById('userBio');
const nativeSttContainer = document.getElementById('nativeSttContainer');
const useNativeSttToggle = document.getElementById('useNativeSttToggle');
const aiProvider = document.getElementById('aiProvider');
const geminiSettings = document.getElementById('geminiSettings');
const geminiApiKey = document.getElementById('geminiApiKey');
const aiModel = document.getElementById('aiModel');
const aiVoice = document.getElementById('aiVoice');
const storageMode = document.getElementById('storageMode');
const saveSettingsBtn = document.getElementById('saveSettingsBtn');

// Dynamically inject Mic Profile UI dropdown
const micProfileContainer = document.createElement('div');
micProfileContainer.style.display = 'flex';
micProfileContainer.style.alignItems = 'center';
micProfileContainer.style.margin = '12px 0';
micProfileContainer.innerHTML = `
    <span style="flex-grow:1; color:var(--on-surface);">Mic Profile</span>
    <select id="micProfileSelect" style="padding:4px; border-radius:4px; background:var(--surface-variant); color:var(--on-surface); border:1px solid var(--outline);">
        <option value="standard">Standard</option>
        <option value="adaptive">Adaptive Filtering</option>
        <option value="heavy">Heavy Filtering</option>
        <option value="mute_playback">Mute on Playback</option>
    </select>
`;
if (storageMode) storageMode.parentNode.insertBefore(micProfileContainer, storageMode.nextSibling);
const micProfileSelect = document.getElementById('micProfileSelect');
let currentMicProfile = localStorage.getItem('speax_mic_profile') || 'standard';
if (micProfileSelect) micProfileSelect.value = currentMicProfile;
micProfileSelect.addEventListener('change', () => {
    currentMicProfile = micProfileSelect.value;
    localStorage.setItem('speax_mic_profile', currentMicProfile);

    // If we're recording, we might need to restart it to apply new device constraints
    if (inContext && inContext.state !== 'closed' && (currentMicProfile === 'heavy' || localStorage.getItem('speax_mic_profile') === 'heavy')) {
        // Need to ask for new constraints from getUserMedia if switching to/from heavy
    }
});

// Dynamically inject Local TTS UI toggle
const localTtsContainer = document.createElement('div');
localTtsContainer.style.display = 'flex';
localTtsContainer.style.alignItems = 'center';
localTtsContainer.style.margin = '12px 0';
localTtsContainer.innerHTML = `
    <span style="flex-grow:1; color:var(--on-surface);">Use Local TTS</span>
    <input type="checkbox" id="useLocalTtsToggle">
`;
if (micProfileContainer) micProfileContainer.parentNode.insertBefore(localTtsContainer, micProfileContainer.nextSibling);
const useLocalTtsToggle = document.getElementById('useLocalTtsToggle');
let useLocalTts = localStorage.getItem('speax_local_tts') === 'true';
if (useLocalTtsToggle) useLocalTtsToggle.checked = useLocalTts;

// Dynamically inject Passive Assistant UI toggle
const passiveAssistantContainer = document.createElement('div');
passiveAssistantContainer.style.display = 'flex';
passiveAssistantContainer.style.alignItems = 'center';
passiveAssistantContainer.style.margin = '12px 0';
passiveAssistantContainer.innerHTML = `
    <span style="flex-grow:1; color:var(--on-surface);" title="Only responds when addressed by name (e.g. Alyx)">Passive Assistant</span>
    <input type="checkbox" id="passiveAssistantToggle">
`;
localTtsContainer.parentNode.insertBefore(passiveAssistantContainer, localTtsContainer.nextSibling);
const passiveAssistantToggle = document.getElementById('passiveAssistantToggle');
let passiveAssistant = localStorage.getItem('speax_passive_assistant') === 'true';
if (passiveAssistantToggle) passiveAssistantToggle.checked = passiveAssistant;


aiProvider.value = localStorage.getItem('speax_provider') || 'ollama';
geminiApiKey.value = localStorage.getItem('speax_gemini_key') || '';
userName.value = localStorage.getItem('speax_user_name') || '';
userBio.value = localStorage.getItem('speax_user_bio') || '';
if (aiProvider.value === 'gemini') geminiSettings.style.display = 'flex';
storageMode.value = localStorage.getItem('speax_client_storage') === 'true' ? 'client' : 'server';

const memoryManager = new MemoryManager();
let serverClient = null; // initialized below

async function loadModels() {
    const provider = aiProvider.value;
    const apiKey = geminiApiKey.value;

    if (provider === 'gemini' && !apiKey) {
        aiModel.innerHTML = '<option value="">Enter API Key first...</option>';
        return;
    }

    aiModel.innerHTML = '<option value="">Loading models...</option>';
    try {
        const res = await fetch(`/api/models?provider=${provider}&apiKey=${apiKey}`);
        const models = await res.json();
        aiModel.innerHTML = '';
        if (models && models.length > 0) {
            models.forEach(m => {
                const opt = document.createElement('option');
                opt.value = m.id;
                opt.innerText = m.name;
                aiModel.appendChild(opt);
            });
            const savedModel = localStorage.getItem('speax_model');
            if (savedModel && Array.from(aiModel.options).some(opt => opt.value === savedModel)) {
                aiModel.value = savedModel;
            }
        } else {
            aiModel.innerHTML = '<option value="">No models found</option>';
        }
    } catch (e) {
        aiModel.innerHTML = '<option value="">Error loading models</option>';
    }
}

async function loadVoices() {
    aiVoice.innerHTML = '<option value="">Loading voices...</option>';
    try {
        const res = await fetch('/api/voices');
        const voices = await res.json();
        aiVoice.innerHTML = '';
        if (voices && voices.length > 0) {
            voices.forEach(v => {
                const opt = document.createElement('option');
                opt.value = v;
                opt.innerText = v.replace('.onnx', '');
                aiVoice.appendChild(opt);
            });
            const savedVoice = localStorage.getItem('speax_voice');
            if (savedVoice && Array.from(aiVoice.options).some(opt => opt.value === savedVoice)) {
                aiVoice.value = savedVoice;
            }
        } else {
            aiVoice.innerHTML = '<option value="">No voices found</option>';
        }
    } catch (e) {
        aiVoice.innerHTML = '<option value="">Error loading voices</option>';
    }
}

// Initialize Web Speech API if supported
if (isNativeSttSupported) {
    nativeSttContainer.style.display = 'flex';
    useNativeStt = localStorage.getItem('speax_use_native_stt') !== 'false'; // Default true
    useNativeSttToggle.checked = useNativeStt;

    recognition = new SpeechRecognition();
    recognition.continuous = true;
    recognition.interimResults = true;

    recognition.onstart = () => {
        isRecognitionActive = true;
        if (!status.innerText.startsWith("Heard:")) {
            status.innerText = "Listening (Browser)...";
        }
    };

    recognition.onresult = (event) => {
        let interimTranscript = '';
        let finalTranscript = '';

        for (let i = event.resultIndex; i < event.results.length; ++i) {
            const text = event.results[i][0].transcript;
            if (event.results[i].isFinal) {
                finalTranscript += text;
            } else {
                interimTranscript += text;
            }
        }

        if (interimTranscript) {
            status.innerText = `Hearing: ${interimTranscript}`;
        }

        // True Barge-in: Suspend TTS immediately if we hear words (including Web Speech API)
        const isAiActive = (currentAiDiv !== null || isPlayingAudio || window.speechSynthesis.speaking);
        if ((interimTranscript || finalTranscript) && isAiActive && !isPaused) {
            stopAudio();
            window.speechSynthesis.cancel(); // Barge-in = stop talking!
        }

        if (finalTranscript) {
            const cleanText = finalTranscript.trim();

            if (nativeSttBuffer && cleanText.toLowerCase().startsWith(nativeSttBuffer.toLowerCase())) {
                // Android Chrome Bug: The engine passes the entire cumulative sentence history 
                // in the newest chunk instead of just the delta. We overwrite to prevent duplication.
                nativeSttBuffer = cleanText;
            } else {
                // Desktop Chrome: Standard behavior, returns discrete new words.
                nativeSttBuffer = nativeSttBuffer ? `${nativeSttBuffer} ${cleanText}` : cleanText;
            }

            status.innerText = `Heard: ${nativeSttBuffer}`;
        }

        clearTimeout(nativeSttDebounceTimer);
        nativeSttDebounceTimer = setTimeout(flushNativeSttBuffer, 1500);
    };

    recognition.onerror = (event) => {
        if (event.error === 'not-allowed') {
            useNativeStt = false;
            useNativeSttToggle.checked = false;
            localStorage.setItem('speax_use_native_stt', 'false');
        }
    };

    recognition.onend = () => {
        isRecognitionActive = false;
        // Do NOT flush here! Let the debounce timer naturally stitch sentences together across Chrome disconnects.
        // Auto-restart loop (like Android continuous dictation)
        if (isDictationActive && !isMuted && !isPaused && useNativeStt && serverClient && serverClient.isOpen()) {
            setTimeout(startNativeListening, 200);
        }
    };

    useNativeSttToggle.onchange = (e) => {
        useNativeStt = e.target.checked;
        localStorage.setItem('speax_use_native_stt', useNativeStt);

        if (serverClient && serverClient.isOpen() && !isMuted && !isPaused) {
            if (useNativeStt) {
                stopRecording(); // Tear down custom VAD
                startNativeListening();
            } else {
                stopNativeListening(); // Tear down Web STT
                flushNativeSttBuffer();
                startRecording();
            }
        }
    };
}

function flushNativeSttBuffer() {
    if (nativeSttBuffer && serverClient && serverClient.isOpen()) {
        serverClient.send(`[TEXT_PROMPT:${Date.now()}]:${nativeSttBuffer}`);
        stopAudio();
        status.innerText = `Heard: ${nativeSttBuffer}`;

        clearTimeout(statusResetTimeout);
        statusResetTimeout = setTimeout(() => {
            if (status.innerText.startsWith("Heard:")) {
                status.innerText = "Listening (Browser)...";
            }
        }, 3000);
    } else if (!nativeSttBuffer && (currentAiDiv !== null || isPlayingAudio) && !isPaused) {
        // False alarm (e.g. cough or background noise): Resume TTS!
        if (outContext && outContext.state === 'suspended') {
            outContext.resume();
        }
    }
    nativeSttBuffer = "";
}

// --- Local Edge TTS Processing Queue ---
let localTtsQueue = [];
let isGeneratingLocalTts = false;

async function processLocalTtsQueue() {
    if (isGeneratingLocalTts || localTtsQueue.length === 0) return;
    isGeneratingLocalTts = true;

    const text = localTtsQueue.shift();
    console.log("[Local TTS] Synthesizing chunk:", text);

    try {
        await new Promise((resolve) => {
            const utterance = new SpeechSynthesisUtterance(text);

            // Optional: If you ever select a native browser voice in the dropdown, it will try to use it!
            const voices = window.speechSynthesis.getVoices();
            const selectedVoice = voices.find(v => v.name === aiVoice.value);
            if (selectedVoice) utterance.voice = selectedVoice;

            utterance.onend = resolve;
            utterance.onerror = resolve; // Resolve on error so the queue doesn't jam

            window.speechSynthesis.speak(utterance);
        });
    } catch (err) {
        console.error("Local TTS Error:", err);
    }

    isGeneratingLocalTts = false;
    processLocalTtsQueue(); // Check for next chunk
}

function closeDrawers() {
    threadsSidebar.style.bottom = '-100%';
    settingsSidebar.style.left = '-320px';
    drawerOverlay.classList.remove('active');
}

drawerOverlay.onclick = closeDrawers;
threadsFab.onclick = () => { threadsSidebar.style.bottom = '0'; drawerOverlay.classList.add('active'); };
closeThreadsBtn.onclick = closeDrawers;
closeSettingsBtn.onclick = closeDrawers;
aiProvider.onchange = () => { geminiSettings.style.display = aiProvider.value === 'gemini' ? 'flex' : 'none'; loadModels(); };
geminiApiKey.onblur = () => { if (aiProvider.value === 'gemini') loadModels(); };

saveSettingsBtn.onclick = () => {
    localStorage.setItem('speax_provider', aiProvider.value);
    localStorage.setItem('speax_gemini_key', geminiApiKey.value);
    localStorage.setItem('speax_model', aiModel.value);
    localStorage.setItem('speax_voice', aiVoice.value);
    localStorage.setItem('speax_user_name', userName.value);
    localStorage.setItem('speax_user_bio', userBio.value);

    if (useLocalTtsToggle) {
        useLocalTts = useLocalTtsToggle.checked;
        localStorage.setItem('speax_local_tts', useLocalTts);
    }

    if (passiveAssistantToggle) {
        passiveAssistant = passiveAssistantToggle.checked;
        localStorage.setItem('speax_passive_assistant', passiveAssistant);
    }

    const isClient = storageMode.value === 'client';
    const wasClient = memoryManager.isClientSide;

    if (isClient !== wasClient && serverClient && serverClient.isOpen()) {
        if (isClient) {
            if (confirm("Do you want to transfer all existing server threads to your local browser storage?")) {
                serverClient.sendRequestFullExport();
            }
        } else {
            if (confirm("Do you want to transfer all your local threads to the server?")) {
                serverClient.sendRestoreClientThreads(memoryManager.getFullState());
            }
        }
    }

    memoryManager.setClientSide(isClient);

    closeDrawers();
    if (serverClient && serverClient.isOpen()) {
        serverClient.sendSettings(getSettingsObj());
    }
};

loadModels();
loadVoices();

if (document.cookie.includes('speax_session=')) {
    authSection.style.display = 'none';
    appSection.style.display = 'flex';
    threadsFab.style.display = 'flex';
    userProfileContainer.style.display = 'block';

    const avatarCookie = document.cookie.split('; ').find(row => row.startsWith('speax_avatar='));
    if (avatarCookie) {
        userAvatarBtn.src = decodeURIComponent(avatarCookie.split('=')[1]);
    } else {
        userAvatarBtn.src = 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" fill="%23aaa" viewBox="0 0 24 24"><path d="M12 12c2.21 0 4-1.79 4-4s-1.79-4-4-4-4 1.79-4 4 1.79 4 4 4zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z"/></svg>';
    }
} else {
    authSection.style.display = 'block';
    appSection.style.display = 'none';
    threadsFab.style.display = 'none';
    userProfileContainer.style.display = 'none';
}

menuSettingsBtn.onclick = () => { settingsSidebar.style.left = '0'; drawerOverlay.classList.add('active'); };
menuLogoutBtn.onclick = () => {
    document.cookie = "speax_session=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/;";
    document.cookie = "speax_avatar=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/;";
    window.location.reload();
};

loginBtn.onclick = () => window.location.href = '/auth/login';

const textInputContainer = document.createElement('div');
textInputContainer.className = 'text-input-container';
textInputContainer.innerHTML = `
    <input type="text" id="chatTextInput" placeholder="Type a message..." style="flex-grow: 1; padding: 12px; border-radius: 24px; border: 1px solid var(--border, #444); background: var(--surface, #222); color: var(--on-surface, #fff); outline: none;">
    <button id="chatSendBtn" style="margin-left: 8px; padding: 12px 18px; border-radius: 24px; border: none; background: var(--primary, #00d1c1); color: #000; cursor: pointer; font-weight: bold;">Send</button>
`;
textInputContainer.style.display = 'none';
textInputContainer.style.padding = '12px 16px';
textInputContainer.style.alignItems = 'center';
textInputContainer.style.flexShrink = '0';
transcript.parentNode.insertBefore(textInputContainer, transcript.nextSibling);

const chatTextInput = document.getElementById('chatTextInput');
const chatSendBtn = document.getElementById('chatSendBtn');

function sendTypedMessage() {
    const text = chatTextInput.value.trim();
    if (text && serverClient && serverClient.isOpen()) {
        serverClient.send(`[TYPED_PROMPT:${Date.now()}]:${text}`);
        chatTextInput.value = '';
        stopAudio();
    }
}
chatSendBtn.onclick = sendTypedMessage;
chatTextInput.onkeydown = (e) => { if (e.key === 'Enter') sendTypedMessage(); };

function switchTab(activeBtn, activeView) {
    [homeTabBtn, threadTabBtn, memoryTabBtn].forEach(btn => btn?.classList.remove('active'));
    activeBtn?.classList.add('active');

    if (homeView) homeView.style.display = 'none';
    if (transcript) {
        transcript.style.display = 'none';
        textInputContainer.style.display = 'none';
    }
    if (summaryView) summaryView.style.display = 'none';

    if (activeView) {
        activeView.style.display = activeView === homeView ? 'flex' : 'block';
        if (activeView === transcript) {
            textInputContainer.style.display = 'flex';
            if (threadsFab) threadsFab.style.bottom = '160px'; // Glide up to clear the text input
        } else {
            if (threadsFab) threadsFab.style.bottom = '100px'; // Drop back down to default resting position
        }
    }
}

if (homeTabBtn) homeTabBtn.onclick = () => switchTab(homeTabBtn, homeView);
if (threadTabBtn) threadTabBtn.onclick = () => { switchTab(threadTabBtn, transcript); transcript.scrollTop = transcript.scrollHeight; };
if (memoryTabBtn) memoryTabBtn.onclick = () => switchTab(memoryTabBtn, summaryView);

async function requestWakeLock() {
    try {
        if ('wakeLock' in navigator) {
            wakeLock = await navigator.wakeLock.request('screen');
            console.log('Wake Lock acquired');
        }
    } catch (err) {
        console.error('Wake Lock error:', err);
    }
}

async function releaseWakeLock() {
    if (wakeLock !== null) {
        await wakeLock.release();
        wakeLock = null;
        console.log('Wake Lock released');
    }
}

document.addEventListener('visibilitychange', async () => {
    if (wakeLock !== null && document.visibilityState === 'visible' && isDictationActive) {
        await requestWakeLock();
    }
});

mainToggleBtn.onclick = async () => {
    if (isDictationActive) {
        stopEverything();
    } else {
        isDictationActive = true;
        reconnectDelay = 1000;
        mainToggleBtn.innerHTML = ICON_WAIT;
        mainToggleBtn.className = 'main-toggle-btn processing';
        connectWebSocket();
    }
};

threadNameInput.onblur = () => {
    if (serverClient && serverClient.isOpen()) serverClient.sendRenameThread(threadNameInput.value);
};
threadNameInput.onkeydown = (e) => { if (e.key === 'Enter') threadNameInput.blur(); };

newThreadBtn.onclick = () => {
    const name = prompt("New thread name:", "New Thread");
    if (name && serverClient && serverClient.isOpen()) serverClient.sendNewThread(name);
};

function getSettingsObj() {
    const googleNameMatch = document.cookie.match(/(?:^|; )speax_google_name=([^;]*)/);
    const googleName = googleNameMatch ? decodeURIComponent(googleNameMatch[1]) : '';

    return {
        userName: localStorage.getItem('speax_user_name') || '',
        googleName: localStorage.getItem('speax_google_name') || googleName,
        userBio: localStorage.getItem('speax_user_bio') || '',
        provider: localStorage.getItem('speax_provider') || 'ollama',
        apiKey: localStorage.getItem('speax_gemini_key') || '',
        model: localStorage.getItem('speax_model') || '',
        voice: localStorage.getItem('speax_voice') || '',
        clientStorage: memoryManager.isClientSide,
        clientTts: useLocalTts,
        passiveAssistant: passiveAssistant
    };
}

function renderMemoryTab() {
    if (!summaryContent) return;
    const data = lastSummaryData;
    const ctxPct = Math.min(100, (data.estTokens / data.maxTokens) * 100) || 0;
    const arcPct = Math.min(100, (data.archiveTurns / data.maxArchiveTurns) * 100) || 0;

    let tokenHtml = '';
    const usageKeys = Object.keys(lastTokenUsage);
    if (usageKeys.length > 0) {
        tokenHtml = `<div class="memory-title" >API Token Usage:</div><div class="memory-box">`;
        usageKeys.forEach(k => {
            const displayKey = (k === 'ollama' || k === 'default') ? 'Local (Ollama)' : (k.length > 10 ? `Key: ...${k.slice(-4)}` : k);
            const tokens = lastTokenUsage[k].toLocaleString();
            tokenHtml += `<div class="memory-stat-row" style="margin-bottom: 4px;"><span>${displayKey}</span><span style="color: var(--primary, #00d1c1); font-weight: bold;">${tokens}</span></div>`;
        });
        tokenHtml += `</div>`;
    }

    summaryContent.innerHTML = `
        <div class="memory-box">
            <div style="margin-bottom: 12px;">
                <div class="memory-stat-row"><span>Active Context Est. (Tokens)</span><span>${data.estTokens.toLocaleString()} / ${data.maxTokens.toLocaleString()}</span></div>
                <div class="memory-bar-track"><div class="memory-bar-fill-ctx" style="width: ${ctxPct}%;"></div></div>
            </div>
            <div>
                <div class="memory-stat-row" ><span>Archive Capacity (Turns)</span><span>${data.archiveTurns} / ${data.maxArchiveTurns}</span></div>
                <div class="memory-bar-track" style="margin-bottom:0;"><div class="memory-bar-fill-arc" style="width: ${arcPct}%;"></div></div>
            </div>
        </div>
        ${tokenHtml}
        <div class="memory-title">Summary of ${data.archiveTurns} older turns:</div>
        <div class="memory-body">${data.text || "No summary generated yet."}</div>
    `;
}

function startNativeListening() {
    if (!recognition || !useNativeStt || isMuted || isPaused || !isDictationActive) return;
    if (!isRecognitionActive) {
        try { recognition.start(); } catch (e) { }
    }
}

function stopNativeListening() {
    if (!recognition) return;
    try { recognition.stop(); } catch (e) { }
    clearTimeout(nativeSttDebounceTimer);
}

const deviceInfo = await getDeviceInfo();

serverClient = new ServerClient(
    `${window.location.protocol === 'https:' ? 'wss:' : 'ws:'}//${window.location.host}/ws?client=web&device=${encodeURIComponent(deviceInfo)}`,
    {
        onOpen: () => {
            status.innerText = isMuted ? "Status: Connected - Muted" : "Status: Connected - Listening...";
            mainToggleBtn.innerHTML = ICON_ALYX;
            mainToggleBtn.className = 'main-toggle-btn listening';
            muteBtn.style.display = 'flex';
            muteBtn.innerHTML = isMuted ? ICON_MIC_MUTE : ICON_MIC;
            muteBtn.classList.toggle('pressed', isMuted);
            pauseBtn.style.display = 'flex';
            pauseBtn.innerHTML = isPaused ? ICON_PLAY : ICON_PAUSE;
            pauseBtn.classList.toggle('pressed', isPaused);
            reconnectDelay = 1000;
            requestWakeLock();
            if (useNativeStt) {
                startNativeListening();
            } else {
                if (!processor) startRecording();
            }

            if (memoryManager.isClientSide) {
                // Rehydrate empty server memory with our local reality
                serverClient.sendRestoreClientThreads(memoryManager.getFullState());
            } else {
                serverClient.sendRequestSync();
            }
        },
        onClose: () => {
            status.innerText = "Status: Disconnected";
            stopRecording();
            stopNativeListening();
            flushNativeSttBuffer(); // Don't lose the final thought!
            releaseWakeLock();

            if (isDictationActive) {
                status.innerText = `Status: Reconnecting in ${reconnectDelay / 1000}s...`;
                setTimeout(() => serverClient.connect(), reconnectDelay);
                reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY);
            } else {
                mainToggleBtn.innerHTML = ICON_POWER;
                mainToggleBtn.className = 'main-toggle-btn disconnected';
                mainToggleBtn.style.transform = 'scale(1)';
                mainToggleBtn.style.boxShadow = 'none';
                muteBtn.style.display = 'none';
                pauseBtn.style.display = 'none';
                isPaused = false;
            }
        },
        onAudioChunk: (blob) => {
            const estSecs = Math.max(0, blob.size - 44) / 32000;
            aiAudioTotalSecs += estSecs;
            audioQueue.push(blob);
            if (!isPlayingAudio) playNextAudio();
        },
        onTtsChunk: (text) => {
            localTtsQueue.push(text);
            processLocalTtsQueue();
        },
        onSettingsSync: (s) => {
            if (s.tokenUsage) {
                lastTokenUsage = s.tokenUsage;
                renderMemoryTab();
            }
            if (s.provider) {
                localStorage.setItem('speax_user_name', s.userName || '');
                localStorage.setItem('speax_user_bio', s.userBio || '');
                localStorage.setItem('speax_provider', s.provider);
                localStorage.setItem('speax_model', s.model || '');
                localStorage.setItem('speax_voice', s.voice || '');
                localStorage.setItem('speax_client_storage', s.clientStorage ? 'true' : 'false');
                localStorage.setItem('speax_google_name', s.googleName || '');

                userName.value = s.userName || '';
                userBio.value = s.userBio || '';
                aiProvider.value = s.provider;
                storageMode.value = s.clientStorage ? 'client' : 'server';
                useLocalTts = s.clientTts || false;
                if (useLocalTtsToggle) useLocalTtsToggle.checked = useLocalTts;
                passiveAssistant = s.passiveAssistant || false;
                if (passiveAssistantToggle) passiveAssistantToggle.checked = passiveAssistant;
                geminiSettings.style.display = s.provider === 'gemini' ? 'flex' : 'none';
                memoryManager.setClientSide(s.clientStorage);

                loadModels().then(() => { if (s.model) aiModel.value = s.model; });
                loadVoices().then(() => { if (s.voice) aiVoice.value = s.voice; });
            }
        },
        onThreadsSync: (data) => {
            const safeThreads = data.threads || [];
            threadNameInput.value = safeThreads.find(t => t.id === data.activeId)?.name || 'General Chat';
            threadList.innerHTML = '';
            safeThreads.forEach(t => {
                const btn = document.createElement('div');
                btn.className = `thread-item ${t.id === data.activeId ? 'active' : ''}`;

                const nameSpan = document.createElement('span');
                nameSpan.innerText = t.name;
                nameSpan.style.flexGrow = '1';
                nameSpan.onclick = () => {
                    if (t.id !== data.activeId) serverClient.sendSwitchThread(t.id);
                    closeDrawers();
                };

                const delBtn = document.createElement('button');
                delBtn.className = 'btn-del-circle';
                delBtn.innerText = 'X';
                delBtn.onclick = (e) => {
                    e.stopPropagation();
                    if (confirm('Delete this thread?')) serverClient.sendDeleteThread(t.id);
                };

                btn.appendChild(nameSpan);
                btn.appendChild(delBtn);
                threadList.appendChild(btn);
            });
            memoryManager.updateThreads(data.activeId, safeThreads);
        },
        onHistorySync: (data) => {
            const prevScrollTop = transcript.scrollTop;
            const wasAtBottom = transcript.scrollHeight - transcript.scrollTop <= transcript.clientHeight + 50;

            transcript.innerHTML = '';
            const combined = [...(data.archive || []), ...(data.history || [])];

            combined.forEach((msg, idx) => {
                const msgDiv = document.createElement('div');
                msgDiv.className = 'msg-row';

                let delBtn = null;
                if (msg.role !== 'assistant' && msg.role !== 'system') {
                    delBtn = document.createElement('button');
                    delBtn.className = 'btn-del-circle';
                    delBtn.innerText = 'X';
                    delBtn.title = 'Delete this interaction';
                    delBtn.onclick = () => {
                        serverClient.sendDeleteMsg(idx);
                        // Optimistically remove the pair from the DOM instantly
                        const nextEl = msgDiv.nextElementSibling;
                        msgDiv.remove();
                        if (nextEl && nextEl.querySelector('span') && nextEl.querySelector('span').innerText.includes('Alyx:')) nextEl.remove();
                    };
                }

                const contentSpan = document.createElement('span');
                contentSpan.className = 'msg-content';
                if (msg.role === 'assistant') {
                    contentSpan.classList.add('msg-assistant');
                    contentSpan.innerText = `Alyx: ${msg.content}`;
                } else if (msg.role === 'system') {
                    contentSpan.classList.add('msg-system');
                    contentSpan.style.color = '#888';
                    contentSpan.style.fontStyle = 'italic';
                    contentSpan.innerText = `[System]: ${msg.content}`;
                } else {
                    contentSpan.classList.add('msg-user');
                    const gNameMatch = document.cookie.match(/(?:^|; )speax_google_name=([^;]*)/);
                    const gName = localStorage.getItem('speax_google_name') || (gNameMatch ? decodeURIComponent(gNameMatch[1]) : 'User');
                    const uName = userName.value || gName;
                    contentSpan.innerText = `${uName}: ${msg.content}`;
                }

                msgDiv.appendChild(contentSpan);
                if (delBtn) msgDiv.appendChild(delBtn);
                transcript.appendChild(msgDiv);
            });

            if (wasAtBottom) transcript.scrollTop = transcript.scrollHeight;
            else transcript.scrollTop = prevScrollTop;

            memoryManager.updateActiveMemory(data.history, data.archive, undefined);
        },
        onSummarySync: (data) => {
            try {
                lastSummaryData = data;
                renderMemoryTab();
            } catch (e) {
                summaryContent.innerText = data.text || "No summary generated yet.";
            }
            memoryManager.updateActiveMemory(undefined, undefined, data.text || "");
        },
        onToolUIEvent: (eventData) => {
            const container = document.getElementById('toolToastContainer');
            if (!container) return;

            const { executionId, toolName, actionName, status: eventStatus, summary } = eventData;

            // Look for existing toast for this executionId
            let toast = document.getElementById(`tool-toast-${executionId}`);

            if (!toast) {
                toast = document.createElement('div');
                toast.id = `tool-toast-${executionId}`;
                toast.style.padding = '8px 16px';
                toast.style.borderRadius = '20px';
                toast.style.background = 'var(--surface-high, #333)';
                toast.style.color = 'var(--text-bright, #fff)';
                toast.style.fontSize = '0.9em';
                toast.style.boxShadow = '0 2px 8px rgba(0,0,0,0.3)';
                toast.style.display = 'flex';
                toast.style.alignItems = 'center';
                toast.style.gap = '8px';
                toast.style.transition = 'opacity 0.3s ease-out, transform 0.3s ease-out';
                toast.innerHTML = `
                    <div class="spinner" style="width: 14px; height: 14px; border: 2px solid rgba(0,209,193,0.3); border-top-color: #00D1C1; border-radius: 50%; animation: spin 1s linear infinite;"></div>
                    <span>Using <b>${toolName}</b>...</span>
                `;
                container.appendChild(toast);

                // Add spin animation dynamically if not present
                if (!document.getElementById('spin-keyframes')) {
                    const style = document.createElement('style');
                    style.id = 'spin-keyframes';
                    style.innerHTML = `@keyframes spin { to { transform: rotate(360deg); } }`;
                    document.head.appendChild(style);
                }
            }

            if (eventStatus === 'success' || eventStatus === 'error') {
                // Update final state
                const isError = eventStatus === 'error';
                const icon = isError ? '❌' : '✅';
                const color = isError ? '#ff4d4d' : '#00D1C1';

                toast.style.background = isError ? 'var(--surface-error, #400)' : 'var(--surface-high, #333)';
                toast.innerHTML = `
                    <span style="color: ${color}; font-size: 1.1em;">${icon}</span>
                    <span>${summary || (isError ? 'Tool failed' : 'Tool finished')}</span>
                `;

                // Remove after delay
                setTimeout(() => {
                    toast.style.opacity = '0';
                    toast.style.transform = 'translateY(-10px)';
                    setTimeout(() => toast.remove(), 300);
                }, 4000);
            }
        },
        onFullExportSync: (data) => {
            memoryManager.activeId = data.activeId;
            memoryManager.threads = data.threads || {};
            memoryManager.saveToDisk();
            alert("Transfer complete. Your threads are now saved locally.");
        },
        onAiStart: () => {
            aiAudioTotalSecs = 0;
            aiAudioPlayedSecs = 0;
            localTtsQueue = []; // Clear pending TTS queue
            currentVisualPercent = 0;
            progressBar.style.transition = 'none';
            progressBar.style.width = '0%';
            currentAiDiv = document.createElement('div');
            currentAiDiv.className = 'msg-content msg-assistant';
            currentAiDiv.innerText = 'Alyx: ';
            transcript.appendChild(currentAiDiv);
            transcript.scrollTop = transcript.scrollHeight;
        },
        onAiEnd: () => {
            currentAiDiv = null;
        },
        onAiText: (text) => {
            if (currentAiDiv) currentAiDiv.innerText += text;
            transcript.scrollTop = transcript.scrollHeight;
        },
        onUserText: (text) => {
            const msg = document.createElement('div');
            const gNameMatch = document.cookie.match(/(?:^|; )speax_google_name=([^;]*)/);
            const gName = localStorage.getItem('speax_google_name') || (gNameMatch ? decodeURIComponent(gNameMatch[1]) : 'User');
            const uName = userName.value || gName;
            msg.innerText = `${uName}: ${text}`;
            transcript.appendChild(msg);
            transcript.scrollTop = transcript.scrollHeight;
        },
        onIgnored: () => {
            if (outContext && outContext.state === 'suspended') {
                outContext.resume();
            }
        }
    }
);

function connectWebSocket() {
    if (!isDictationActive) return;
    status.innerText = "Status: Connecting...";
    serverClient.connect();
}

muteBtn.onclick = () => {
    isMuted = !isMuted;
    if (isMuted) {
        muteBtn.innerHTML = ICON_MIC_MUTE;
        muteBtn.classList.add('pressed');
        muteBtn.style.transform = 'scale(1)';
        muteBtn.style.boxShadow = 'none';
        if (useNativeStt) stopNativeListening();
        flushNativeSttBuffer();
        if (isSpeaking && !useNativeStt) {
            isSpeaking = false;
            silenceFrames = 0;
            if (audioChunks.length >= MIN_CHUNKS) {
                status.innerText = "Status: Processing with Whisper...";
                sendAndClearBuffer();
            } else {
                audioChunks = [];
            }
        }
        if (!isPlayingAudio && serverClient.isOpen()) status.innerText = "Status: Connected - Muted";
    } else {
        muteBtn.innerHTML = ICON_MIC;
        muteBtn.classList.remove('pressed');
        if (useNativeStt && isDictationActive && !isPaused) startNativeListening();
        if (!isPlayingAudio && serverClient.isOpen()) {
            status.innerText = "Status: Connected - Listening...";
            if (!inputVisualizerId && inputAnalyser) renderInputVisualizer();
        }
    }
};

clearHistoryBtn.onclick = () => {
    if (serverClient.isOpen()) {
        if (confirm("Are you sure you want to expunge all memory?")) {
            serverClient.sendClearHistory();
            memoryManager.clear();
        }
    }
};

if (rebuildSummaryBtn) {
    rebuildSummaryBtn.onclick = () => {
        if (serverClient.isOpen()) {
            serverClient.sendRebuildSummary();
            rebuildSummaryBtn.innerText = "Rebuilding...";
            setTimeout(() => { rebuildSummaryBtn.innerText = "Rebuild Summary"; }, 3000); // Visual reset
        }
    };
}

function stopEverything() {
    isDictationActive = false; // Prevent auto-reconnect
    serverClient.disconnect();
    localTtsQueue = [];
    mainToggleBtn.innerHTML = ICON_POWER;
    mainToggleBtn.className = 'main-toggle-btn disconnected';
    muteBtn.style.display = 'none';
    pauseBtn.style.display = 'none';
    isPaused = false;
    releaseWakeLock();
    stopRecording();
    stopNativeListening();
    flushNativeSttBuffer();
    stopAudio();
}

pauseBtn.onclick = () => {
    isPaused = !isPaused;
    if (isPaused) {
        pauseBtn.innerHTML = ICON_PLAY;
        pauseBtn.classList.add('pressed');
        if (outContext && outContext.state === 'running') outContext.suspend();
        window.speechSynthesis.pause(); // Pause native speech
        if (!isMuted) muteBtn.click(); // Mute mic
        if (useNativeStt) stopNativeListening();
        flushNativeSttBuffer(); // Force send any pending thoughts
        if (audioChunks.length > 0) sendAndClearBuffer(); // Flush any pending audio
        localTtsQueue = []; // Clear pending TTS chunks
        status.innerText = "Status: Paused";
    } else {
        pauseBtn.innerHTML = ICON_PAUSE;
        pauseBtn.classList.remove('pressed');
        if (outContext && outContext.state === 'suspended') outContext.resume();
        window.speechSynthesis.resume(); // Resume native speech
        if (useNativeStt && !isMuted && isDictationActive) startNativeListening();
        if (isMuted) muteBtn.click(); // Unmute mic
        if (!isPlayingAudio && audioQueue.length > 0) playNextAudio(); // Drain the accumulated buffer
        if (isPlayingAudio && !progressAnimId) updateProgressBar(); // Resume progress UI
    }
};

async function playNextAudio() {
    if (audioQueue.length === 0 || isPaused) {
        if (isPlayingAudio && !isPaused && speaxWebSocket && speaxWebSocket.readyState === WebSocket.OPEN) {
            speaxWebSocket.send("[PLAYBACK_COMPLETE]");
        }
        isPlayingAudio = false;
        progressBar.style.width = '0%';
        status.innerText = isMuted ? "Status: Connected - Muted" : "Status: Connected - Listening...";
        return;
    }

    isPlayingAudio = true;
    status.innerText = "Status: Playing Audio...";

    if (!outContext) outContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
    if (outContext.state === 'suspended') await outContext.resume();

    try {
        const blob = audioQueue.shift();
        const estSecs = Math.max(0, blob.size - 44) / 32000;
        const arrayBuffer = await blob.arrayBuffer();
        const audioBuffer = await outContext.decodeAudioData(arrayBuffer);

        // Correct our rough estimate with the precise decoded duration
        console.log(`[Audio Play] Decoded exact duration: ${audioBuffer.duration.toFixed(2)}s (Estimate was ${estSecs.toFixed(2)}s).`);
        aiAudioTotalSecs = aiAudioTotalSecs - estSecs + audioBuffer.duration;

        if (!audioAnalyser) {
            audioAnalyser = outContext.createAnalyser();
            audioAnalyser.fftSize = 256;
            audioAnalyser.connect(outContext.destination);
        }

        const source = outContext.createBufferSource();
        currentAudioSource = source;
        source.buffer = audioBuffer;
        source.connect(audioAnalyser);
        source.onended = () => {
            currentAudioSource = null;
            aiAudioPlayedSecs += aiChunkDuration;
            playNextAudio();
        }; // trigger the next chunk gaplessly!
        source.start(0);

        aiChunkStartContextTime = outContext.currentTime;
        aiChunkDuration = audioBuffer.duration;
        if (!progressAnimId) updateProgressBar();

        if (!visualizerId) renderVisualizer();
    } catch (err) {
        console.error("Error decoding TTS audio:", err);
        playNextAudio();
    }
}

function updateProgressBar() {
    if (!isPlayingAudio || isPaused) {
        progressAnimId = null;
        return;
    }

    progressBar.style.transition = 'none'; // Prevent CSS fighting requestAnimationFrame

    let currentChunkProgress = outContext.currentTime - aiChunkStartContextTime;
    if (currentChunkProgress < 0) currentChunkProgress = 0;
    if (currentChunkProgress > aiChunkDuration) currentChunkProgress = aiChunkDuration;

    let totalPlayed = aiAudioPlayedSecs + currentChunkProgress;
    let targetPercent = aiAudioTotalSecs > 0 ? 100 - ((totalPlayed / aiAudioTotalSecs) * 100) : 0;
    if (targetPercent < 0) targetPercent = 0;
    if (targetPercent > 100) targetPercent = 100;

    // Lerp magic: Move visual percent 10% of the way to the target percent every frame
    currentVisualPercent += (targetPercent - currentVisualPercent) * 0.1;

    // if (progressLogThrottler++ % 15 === 0) {
    //         console.log(`[Progress UI] Target: ${targetPercent.toFixed(1)}% | Visual: ${currentVisualPercent.toFixed(1)}%`);
    // }

    progressBar.style.width = `${currentVisualPercent}%`;

    progressAnimId = requestAnimationFrame(updateProgressBar);
}

function renderInputVisualizer() {
    if (!isDictationActive || !inputAnalyser || isMuted) {
        muteBtn.style.transform = 'scale(1)';
        muteBtn.style.boxShadow = 'none';
        inputVisualizerId = null;
        return;
    }

    const dataArray = new Uint8Array(inputAnalyser.frequencyBinCount);
    inputAnalyser.getByteFrequencyData(dataArray);

    let sum = 0;
    for (let i = 0; i < dataArray.length; i++) sum += dataArray[i];
    const avg = sum / dataArray.length; // 0 to 255

    const scale = 1 + (avg / 255) * 0.15;
    const shadow = (avg / 255) * 40;

    muteBtn.style.transform = `scale(${scale})`;
    // Pulse Blue for User Input (14, 99, 156 is the RGB for #0E639C)
    muteBtn.style.boxShadow = `0 0 ${shadow}px ${shadow / 2}px rgba(14, 99, 156, 0.8)`;

    inputVisualizerId = requestAnimationFrame(renderInputVisualizer);
}

function renderVisualizer() {
    if (!isPlayingAudio || !audioAnalyser || !isDictationActive) {
        visualizerId = null;
        mainToggleBtn.style.transform = 'scale(1)';
        mainToggleBtn.style.boxShadow = 'none';
        return;
    }

    const dataArray = new Uint8Array(audioAnalyser.frequencyBinCount);
    audioAnalyser.getByteFrequencyData(dataArray);

    let sum = 0;
    for (let i = 0; i < dataArray.length; i++) sum += dataArray[i];
    const avg = sum / dataArray.length; // 0 to 255

    // Map amplitude to scale (1 to 1.15) and glow size
    const scale = 1 + (avg / 255) * 0.15;
    const shadow = (avg / 255) * 60;

    mainToggleBtn.style.transform = `scale(${scale})`;
    mainToggleBtn.style.boxShadow = `0 0 ${shadow}px ${shadow / 2}px rgba(0, 209, 193, 0.8)`;

    visualizerId = requestAnimationFrame(renderVisualizer);
}

function stopAudio() {
    audioQueue = []; // Clear the pending playlist
    window.speechSynthesis.cancel(); // Kill native Web Speech API if it's running
    if (currentAudioSource) {
        currentAudioSource.onended = null; // Prevent race condition injecting time into the next AI generation!
        try { currentAudioSource.stop(); } catch (e) { }
        currentAudioSource = null;
    }
    isPlayingAudio = false;
    currentVisualPercent = 0;
    progressBar.style.transition = 'none';
    progressBar.style.width = '0%';
    aiAudioTotalSecs = 0;
    aiAudioPlayedSecs = 0;

    if (outContext && outContext.state === 'suspended') {
        outContext.resume(); // Ensure it isn't locked up for the new response
    }
}

let duckingNode = null;
let duckingInterval = null;

function manageDuckingState() {
    if (!duckingNode || !inContext) return;
    const isAiActive = (currentAiDiv !== null || isPlayingAudio || window.speechSynthesis.speaking);
    const now = inContext.currentTime;

    const duckingActive = duckingNode.duckingActive || false;

    let targetGain = 0.1;
    if (currentMicProfile === 'mute_playback') {
        targetGain = 0.0;
    } else if (currentMicProfile === 'adaptive') {
        // Scale Ducking Gain proportionally:
        // Normal baseline (0.05) -> 0.1 target
        // Quiet User (0.01) -> 0.02 target (Heavier Ducking)
        // Loud User (0.1) -> 0.2 target (Lighter Ducking)
        targetGain = Math.max(0.01, Math.min(0.5, (averageSpeechRms / 0.05) * 0.1));
    }

    if (isAiActive && !duckingActive) {
        duckingNode.duckingActive = true;
        duckingNode.gain.cancelScheduledValues(now);
        duckingNode.gain.setTargetAtTime(targetGain, now, 0.05); // Smooth transition down
    } else if (!isAiActive && duckingActive) {
        duckingNode.duckingActive = false;
        duckingNode.gain.cancelScheduledValues(now);
        duckingNode.gain.setTargetAtTime(1.0, now, 0.05); // Smooth transition up
    } else if (isAiActive && duckingActive) {
        // Handle case where profile was changed mid-playback or adaptive adjusted
        duckingNode.gain.setTargetAtTime(targetGain, now, 0.05);
    }
}

async function startRecording() {
    inContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });

    if (inContext.state === 'suspended') {
        await inContext.resume();
    }

    let constraints = { audio: true };
    if (currentMicProfile === 'heavy') {
        constraints = {
            audio: {
                noiseSuppression: true,
                echoCancellation: true,
                autoGainControl: true
            }
        };
    }

    const stream = await navigator.mediaDevices.getUserMedia(constraints);
    input = inContext.createMediaStreamSource(stream);

    duckingNode = inContext.createGain();
    duckingNode.gain.value = 1.0;
    duckingNode.duckingActive = false;
    if (!duckingInterval) duckingInterval = setInterval(manageDuckingState, 100);

    let filterNode = null;
    if (currentMicProfile === 'heavy') {
        filterNode = inContext.createBiquadFilter();
        filterNode.type = 'highpass';
        filterNode.frequency.value = 100;
        input.connect(filterNode);
        filterNode.connect(duckingNode);
    } else {
        input.connect(duckingNode);
    }

    // Using ScriptProcessor for simplicity in this MVP; AudioWorklet is preferred for production
    processor = inContext.createScriptProcessor(4096, 1, 1);

    inputAnalyser = inContext.createAnalyser();
    inputAnalyser.fftSize = 256;

    duckingNode.connect(inputAnalyser);
    duckingNode.connect(processor);

    if (!inputVisualizerId) renderInputVisualizer();

    processor.onaudioprocess = (e) => {
        const inputData = e.inputBuffer.getChannelData(0);

        // Calculate volume (RMS)
        let sum = 0;
        for (let i = 0; i < inputData.length; i++) {
            sum += inputData[i] * inputData[i];
        }
        const rms = isMuted ? 0 : Math.sqrt(sum / inputData.length);

        const currentThreshold = currentMicProfile === 'adaptive' ? Math.max(0.005, averageSpeechRms * 0.3) : NOISE_THRESHOLD;

        if (rms > currentThreshold) {
            // Speech detected
            if (!isSpeaking) {
                if (outContext && outContext.state === 'running') {
                    stopAudio(); // completely abort playback to prevent deadlock
                }
                status.innerText = "Status: Recording (Speaking)...";
                isSpeaking = true;
                speechStartTime = Date.now();
                // Prepend the pre-roll buffer to catch the very start of the word
                audioChunks = [...preRollBuffer];
            }
            silenceFrames = 0;
            audioChunks.push(new Float32Array(inputData));
        } else if (isSpeaking) {
            // Silence detected during a recording
            audioChunks.push(new Float32Array(inputData)); // keep trailing silence
            silenceFrames++;

            if (silenceFrames >= SILENCE_FRAMES_LIMIT) {
                // We are done speaking
                isSpeaking = false;
                silenceFrames = 0;

                if (audioChunks.length >= MIN_CHUNKS) {
                    status.innerText = "Status: Processing with Whisper...";
                    sendAndClearBuffer();
                } else {
                    // It was just a mic pop or sniff, discard it
                    audioChunks = [];
                    status.innerText = isMuted ? "Status: Connected - Muted" : "Status: Connected - Listening...";
                }
            }
        } else {
            // Not speaking, maintain a rolling buffer of the last few frames
            preRollBuffer.push(new Float32Array(inputData));
            if (preRollBuffer.length > PRE_ROLL_FRAMES) {
                preRollBuffer.shift(); // Remove the oldest frame
            }
        }
    };

    // ScriptProcessor MUST be connected to destination to fire events!
    processor.connect(inContext.destination);
}

function sendAndClearBuffer() {
    if (!serverClient.isOpen()) {
        audioChunks = []; // Don't hoard memory if socket is dead
        return;
    }

    const totalLength = audioChunks.reduce((acc, val) => acc + val.length, 0);
    const pcmData = new Int16Array(totalLength);
    let offset = 0;
    let sumSquares = 0;

    for (const chunk of audioChunks) {
        for (let i = 0; i < chunk.length; i++) {
            sumSquares += chunk[i] * chunk[i];
            pcmData[offset++] = Math.max(-1, Math.min(1, chunk[i])) * 0x7FFF;
        }
    }

    // Apply Exponential Moving Average (EMA) to baseline for adaptive profile
    if (totalLength > 0) {
        const chunkRms = Math.sqrt(sumSquares / totalLength);
        averageSpeechRms = (0.8 * averageSpeechRms) + (0.2 * chunkRms);
    }

    // Prepend 8-byte BigEndian timestamp (milliseconds)
    const finalBuffer = new Uint8Array(8 + pcmData.byteLength);
    const view = new DataView(finalBuffer.buffer);
    view.setBigUint64(0, BigInt(speechStartTime));
    finalBuffer.set(new Uint8Array(pcmData.buffer), 8);

    serverClient.send(finalBuffer.buffer);
    audioChunks = [];
}

function stopRecording() {
    if (duckingInterval) {
        clearInterval(duckingInterval);
        duckingInterval = null;
    }
    if (duckingNode) {
        duckingNode.disconnect();
        duckingNode = null;
    }
    if (input && input.mediaStream) {
        input.mediaStream.getTracks().forEach(track => track.stop());
    }
    if (input) {
        input.disconnect();
        input = null;
    }
    if (processor) {
        processor.disconnect();
        processor = null;
    }
    if (inContext && inContext.state !== 'closed') {
        inContext.close();
        inContext = null;
    }
}
