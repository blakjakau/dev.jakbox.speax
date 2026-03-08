let socket;
let audioContext;
let processor;
let input;

let audioChunks = [];
let audioQueue = [];
let isPlayingAudio = false;
let currentAiDiv = null;
let currentAudioSource = null;
let preRollBuffer = [];
let isSpeaking = false;
let silenceFrames = 0;
let wakeLock = null;

const NOISE_THRESHOLD = 0.015; // RMS threshold. Increase if your room is noisy!
const SILENCE_FRAMES_LIMIT = 6; // ~0.75 seconds of trailing silence to stop
const PRE_ROLL_FRAMES = 2; // ~0.5 seconds of audio to keep BEFORE speech is detected
const MIN_CHUNKS = 2; // Require at least ~0.5 seconds of audio to bother sending

const status = document.getElementById('status');
const startBtn = document.getElementById('start');
const stopBtn = document.getElementById('stop');
const transcript = document.getElementById('transcript');

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
    if (wakeLock !== null && document.visibilityState === 'visible' && startBtn.disabled) {
        await requestWakeLock();
    }
});

startBtn.onclick = async () => {
    socket = new WebSocket(`wss://${window.location.host}/ws`);
    
    socket.onopen = () => {
        status.innerText = "Status: Connected - Listening...";
        startBtn.disabled = true;
        stopBtn.disabled = false;
        requestWakeLock();
        startRecording();
    };

    socket.onmessage = async (event) => {
        // If the server sends us a Blob (Binary), it's Text-to-Speech audio!
        if (event.data instanceof Blob) {
            audioQueue.push(event.data);
            if (!isPlayingAudio) playNextAudio();
            return;
        }

        // Otherwise, it's our transcribed text from Whisper
        status.innerText = "Status: Connected - Listening...";
        
        const rawText = event.data;
        const text = rawText.trim();
        
        if (text === "[AI_START]") {
            currentAiDiv = document.createElement('div');
            currentAiDiv.style.color = '#4ec9b0';
            currentAiDiv.innerText = 'AI: ';
            transcript.appendChild(currentAiDiv);
            return;
        } else if (text === "[AI_END]") {
            currentAiDiv = null;
            return;
        }

        if (currentAiDiv) {
            currentAiDiv.innerText += rawText; // keep the spaces!
        } else if (text) {
            const msg = document.createElement('div');
            msg.innerText = `> ${text}`;
            transcript.appendChild(msg);
        }
        transcript.scrollTop = transcript.scrollHeight;
    };

    socket.onclose = () => {
        status.innerText = "Status: Disconnected";
        stopRecording();
        releaseWakeLock();
    };
};

stopBtn.onclick = () => {
    socket.close();
    startBtn.disabled = false;
    stopBtn.disabled = true;
    releaseWakeLock();
};

async function playNextAudio() {
    if (audioQueue.length === 0) {
        isPlayingAudio = false;
        status.innerText = "Status: Connected - Listening...";
        return;
    }
    
    isPlayingAudio = true;
    status.innerText = "Status: Playing Audio...";
    
    if (!audioContext) audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
    if (audioContext.state === 'suspended') await audioContext.resume();

    try {
        const blob = audioQueue.shift();
        const arrayBuffer = await blob.arrayBuffer();
        const audioBuffer = await audioContext.decodeAudioData(arrayBuffer);
        const source = audioContext.createBufferSource();
        currentAudioSource = source;
        source.buffer = audioBuffer;
        source.connect(audioContext.destination);
        source.onended = () => {
            currentAudioSource = null;
            playNextAudio();
        }; // trigger the next chunk gaplessly!
        source.start(0);
    } catch (err) {
        console.error("Error decoding TTS audio:", err);
        playNextAudio();
    }
}

function stopAudio() {
    audioQueue = []; // Clear the pending playlist
    if (currentAudioSource) {
        try { currentAudioSource.stop(); } catch (e) {}
        currentAudioSource = null;
        isPlayingAudio = false;
    }
}

async function startRecording() {
    audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
    
    if (audioContext.state === 'suspended') {
        await audioContext.resume();
    }

    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    input = audioContext.createMediaStreamSource(stream);
    
    // Using ScriptProcessor for simplicity in this MVP; AudioWorklet is preferred for production
    processor = audioContext.createScriptProcessor(4096, 1, 1);

    processor.onaudioprocess = (e) => {
        const inputData = e.inputBuffer.getChannelData(0);
        
        // Calculate volume (RMS)
        let sum = 0;
        for (let i = 0; i < inputData.length; i++) {
            sum += inputData[i] * inputData[i];
        }
        const rms = Math.sqrt(sum / inputData.length);

        if (rms > NOISE_THRESHOLD) {
            // Speech detected
            if (!isSpeaking) {
                stopAudio(); // Instantly mute the AI
                if (socket && socket.readyState === WebSocket.OPEN) {
                    socket.send("[INTERRUPT]"); // Tell the server to kill the generation
                }
                status.innerText = "Status: Recording (Speaking)...";
                isSpeaking = true;
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
                    status.innerText = "Status: Connected - Listening...";
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

    input.connect(processor);
    processor.connect(audioContext.destination);
}

function sendAndClearBuffer() {
    if (socket.readyState !== WebSocket.OPEN) return;
    
    const totalLength = audioChunks.reduce((acc, val) => acc + val.length, 0);
    const pcmData = new Int16Array(totalLength);
    let offset = 0;
    for (const chunk of audioChunks) {
        for (let i = 0; i < chunk.length; i++) {
            pcmData[offset++] = Math.max(-1, Math.min(1, chunk[i])) * 0x7FFF;
        }
    }
    
    socket.send(pcmData.buffer);
    audioChunks = [];
}

function stopRecording() {
    if (input) input.disconnect();
    if (processor) processor.disconnect();
    if (audioContext) audioContext.close();
}
