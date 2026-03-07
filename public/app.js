let socket;
let audioContext;
let processor;
let input;

let audioChunks = [];
let preRollBuffer = [];
let isSpeaking = false;
let silenceFrames = 0;

const NOISE_THRESHOLD = 0.015; // RMS threshold. Increase if your room is noisy!
const SILENCE_FRAMES_LIMIT = 3; // ~0.75 seconds of trailing silence to stop
const PRE_ROLL_FRAMES = 2; // ~0.5 seconds of audio to keep BEFORE speech is detected
const MIN_CHUNKS = 2; // Require at least ~0.5 seconds of audio to bother sending

const status = document.getElementById('status');
const startBtn = document.getElementById('start');
const stopBtn = document.getElementById('stop');
const transcript = document.getElementById('transcript');

startBtn.onclick = async () => {
    socket = new WebSocket(`ws://${window.location.host}/ws`);
    
    socket.onopen = () => {
        status.innerText = "Status: Connected - Listening...";
        startBtn.disabled = true;
        stopBtn.disabled = false;
        startRecording();
    };

    socket.onmessage = (event) => {
        status.innerText = "Status: Connected - Listening...";
        if (!event.data.trim()) return; // Ignore empty text
        
        const msg = document.createElement('div');
        msg.innerText = `> ${event.data}`;
        transcript.appendChild(msg);
        transcript.scrollTop = transcript.scrollHeight;
    };

    socket.onclose = () => {
        status.innerText = "Status: Disconnected";
        stopRecording();
    };
};

stopBtn.onclick = () => {
    socket.close();
    startBtn.disabled = false;
    stopBtn.disabled = true;
};

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
