#include <jni.h>
#include <string>
#include <aaudio/AAudio.h>
#include <android/log.h>
#include <vector>
#include <mutex>
#include <queue>
#include <thread>
#include <condition_variable>
#include <atomic>
#include "VAD.h"

#define LOG_TAG "NativeAudioEngine"
#define LOGD(...) __android_log_print(ANDROID_LOG_DEBUG, LOG_TAG, __VA_ARGS__)
#define LOGW(...) __android_log_print(ANDROID_LOG_WARN, LOG_TAG, __VA_ARGS__)
#define LOGE(...) __android_log_print(ANDROID_LOG_ERROR, LOG_TAG, __VA_ARGS__)

// --- Forward Declarations ---
struct AudioEngineContext;
aaudio_data_callback_result_t dataCallback(AAudioStream*, void*, void*, int32_t);
void errorCallback(AAudioStream*, void*, aaudio_result_t);

// --- AudioEngineContext ---
class AudioEngineContext {
public:
    AudioEngineContext(JNIEnv* env, jobject obj) : 
        vad(16000, 512), 
        sampleRate(16000), 
        frameSize(512),
        isStreamActive(false),
        workerRunning(true) {
        
        env->GetJavaVM(&javaVM);
        javaObject = env->NewGlobalRef(obj);
        
        jclass clazz = env->GetObjectClass(obj);
        handleSpeechStartID = env->GetMethodID(clazz, "handleSpeechStart", "()V");
        handleStreamingChunkID = env->GetMethodID(clazz, "handleStreamingChunk", "(Ljava/nio/ByteBuffer;IB)V");
        handleSpeechEndID = env->GetMethodID(clazz, "handleSpeechEnd", "(Ljava/nio/ByteBuffer;I)V");
        handleVolumeChangeID = env->GetMethodID(clazz, "handleVolumeChange", "(F)V");

        workerThread = std::thread(&AudioEngineContext::workerLoop, this);
    }

    ~AudioEngineContext() {
        {
            std::lock_guard<std::mutex> lock(queueMutex);
            workerRunning = false;
        }
        queueCv.notify_all();
        if (workerThread.joinable()) workerThread.join();

        JNIEnv* env;
        if (javaVM->GetEnv((void**)&env, JNI_VERSION_1_6) == JNI_OK) {
            env->DeleteGlobalRef(javaObject);
        }
    }

    JavaVM* javaVM;
    jobject javaObject;
    jmethodID handleSpeechStartID;
    jmethodID handleStreamingChunkID;
    jmethodID handleSpeechEndID;
    jmethodID handleVolumeChangeID;

    AAudioStream* stream = nullptr;
    VAD vad;
    int sampleRate;
    int frameSize;
    std::atomic<bool> isStreamActive;
    std::atomic<bool> isMuted{false};
    bool hasTriggeredSpeechStart = false;
    const int CONFIRMATION_SIZE = 16000 * 1; // 1 second min duration
    
    std::vector<int16_t> preRollBuffer;
    const int PRE_ROLL_LIMIT = 10 * 512; // 10 frames (~0.3s) head buffer
    
    std::vector<int16_t> accumulatedChunk;
    const int CHUNK_SIZE_LIMIT = 16000 * 1.5;
    
    enum JniCommandType {
        CMD_SPEECH_START,
        CMD_STREAMING_CHUNK,
        CMD_SPEECH_END,
        CMD_VOLUME_CHANGE,
        CMD_RESTART_STREAM
    };

    struct JniCommand {
        JniCommandType type;
        float volume = 0;
        std::vector<int16_t> audioData;
        int8_t chunkType = 0;
    };

    std::vector<int16_t> vadBuffer;
    const int VAD_FRAME_SIZE = 512;
    int consecutiveSilenceFrames = 0;
    std::atomic<int64_t> lastVolumeReportTime{0};

    std::mutex engineMutex;
    std::mutex bufferMutex;
    std::mutex queueMutex;
    std::condition_variable queueCv;
    std::queue<JniCommand> jniQueue;
    std::thread workerThread;
    std::atomic<bool> workerRunning;

    void workerLoop();
    void restartStream();
};

// --- restartStream ---
void AudioEngineContext::restartStream() {
    LOGD("NativeAudioEngine: Auto-restarting stream after disconnect");
    std::lock_guard<std::mutex> lock(engineMutex);
    
    if (stream) {
        AAudioStream_requestStop(stream);
        AAudioStream_close(stream);
        stream = nullptr;
    }

    {
        std::lock_guard<std::mutex> bLock(bufferMutex);
        vad.reset();
        isStreamActive = false;
        hasTriggeredSpeechStart = false;
        consecutiveSilenceFrames = 0;
        accumulatedChunk.clear();
        preRollBuffer.clear();
        vadBuffer.clear();
    }
    
    AAudioStreamBuilder* builder;
    AAudio_createStreamBuilder(&builder);
    AAudioStreamBuilder_setSampleRate(builder, sampleRate);
    AAudioStreamBuilder_setChannelCount(builder, 1);
    AAudioStreamBuilder_setFormat(builder, AAUDIO_FORMAT_PCM_I16);
    AAudioStreamBuilder_setDirection(builder, AAUDIO_DIRECTION_INPUT);
#if __ANDROID_API__ >= 28
    AAudioStreamBuilder_setUsage(builder, AAUDIO_USAGE_VOICE_COMMUNICATION);
    AAudioStreamBuilder_setContentType(builder, AAUDIO_CONTENT_TYPE_SPEECH);
    
    std::string profile = vad.getMicProfile();
    if (profile == "sensitive") {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_UNPROCESSED);
    } else if (profile == "heavy") {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_VOICE_COMMUNICATION);
    } else {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_VOICE_RECOGNITION);
    }
#endif
    AAudioStreamBuilder_setPerformanceMode(builder, AAUDIO_PERFORMANCE_MODE_LOW_LATENCY);
    AAudioStreamBuilder_setSharingMode(builder, AAUDIO_SHARING_MODE_SHARED);
    AAudioStreamBuilder_setDataCallback(builder, dataCallback, this);
    AAudioStreamBuilder_setErrorCallback(builder, errorCallback, this);

    aaudio_result_t result = AAudioStreamBuilder_openStream(builder, &stream);
    AAudioStreamBuilder_delete(builder);

    if (result == AAUDIO_OK) {
        AAudioStream_requestStart(stream);
        LOGD("NativeAudioEngine: Stream auto-restarted successfully");
    } else {
        LOGE("NativeAudioEngine: Failed to auto-restart stream: %s", AAudio_convertResultToText(result));
        stream = nullptr;
    }
}

// --- workerLoop ---
void AudioEngineContext::workerLoop() {
    JNIEnv* env;
    if (javaVM->AttachCurrentThread(&env, NULL) != 0) {
        LOGE("Failed to attach worker thread to JVM");
        return;
    }

    while (true) {
        JniCommand cmd;
        {
            std::unique_lock<std::mutex> lock(queueMutex);
            queueCv.wait(lock, [this] { return !workerRunning.load() || !jniQueue.empty(); });
            if (!workerRunning.load() && jniQueue.empty()) break;
            cmd = std::move(jniQueue.front());
            jniQueue.pop();
        }

        if (env->ExceptionCheck()) env->ExceptionClear();

        switch (cmd.type) {
            case CMD_SPEECH_START:
                env->CallVoidMethod(javaObject, handleSpeechStartID);
                break;
            case CMD_STREAMING_CHUNK: {
                jobject byteBuffer = env->NewDirectByteBuffer(cmd.audioData.data(), cmd.audioData.size() * 2);
                env->CallVoidMethod(javaObject, handleStreamingChunkID, byteBuffer, (jint)(cmd.audioData.size() * 2), (jbyte)cmd.chunkType);
                env->DeleteLocalRef(byteBuffer);
                break;
            }
            case CMD_SPEECH_END: {
                jobject byteBuffer = env->NewDirectByteBuffer(cmd.audioData.data(), cmd.audioData.size() * 2);
                env->CallVoidMethod(javaObject, handleSpeechEndID, byteBuffer, (jint)(cmd.audioData.size() * 2));
                env->DeleteLocalRef(byteBuffer);
                break;
            }
            case CMD_VOLUME_CHANGE:
                env->CallVoidMethod(javaObject, handleVolumeChangeID, (jfloat)cmd.volume);
                break;
            case CMD_RESTART_STREAM:
                restartStream();
                break;
        }

        if (env->ExceptionCheck()) env->ExceptionClear();
    }

    javaVM->DetachCurrentThread();
    LOGD("JNI Worker thread finished");
}

// --- errorCallback ---
void errorCallback(AAudioStream* stream, void* userData, aaudio_result_t error) {
    LOGE("AAudio Error Callback: %s", AAudio_convertResultToText(error));
    if (error == AAUDIO_ERROR_DISCONNECTED) {
        AudioEngineContext* ctx = (AudioEngineContext*)userData;
        std::lock_guard<std::mutex> lock(ctx->queueMutex);
        AudioEngineContext::JniCommand cmd;
        cmd.type = AudioEngineContext::CMD_RESTART_STREAM;
        ctx->jniQueue.push(std::move(cmd));
        ctx->queueCv.notify_one();
    }
}

// --- dataCallback ---
aaudio_data_callback_result_t dataCallback(
    AAudioStream* stream,
    void* userData,
    void* audioData,
    int32_t numFrames
) {
    AudioEngineContext* ctx = (AudioEngineContext*)userData;
    int16_t* pcm = (int16_t*)audioData;

    for (int i = 0; i < numFrames; ++i) {
        int32_t sample = pcm[i] * 2;
        if (sample > 32767) pcm[i] = 32767;
        else if (sample < -32768) pcm[i] = -32768;
        else pcm[i] = (int16_t)sample;
    }

    {
        std::lock_guard<std::mutex> lock(ctx->bufferMutex);
        ctx->vadBuffer.insert(ctx->vadBuffer.end(), pcm, pcm + numFrames);
    }
    
    if (ctx->isMuted) {
        if (ctx->isStreamActive) {
            ctx->isStreamActive = false;
            std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
            if (ctx->hasTriggeredSpeechStart && !ctx->accumulatedChunk.empty()) {
                std::lock_guard<std::mutex> qLock(ctx->queueMutex);
                AudioEngineContext::JniCommand cmd;
                cmd.type = AudioEngineContext::CMD_SPEECH_END;
                cmd.audioData = std::move(ctx->accumulatedChunk);
                ctx->jniQueue.push(std::move(cmd));
                ctx->queueCv.notify_one();
            }
            ctx->accumulatedChunk.clear();
        }
        {
            std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
            ctx->vadBuffer.clear();
            ctx->preRollBuffer.clear();
        }
        ctx->consecutiveSilenceFrames = 0;
        ctx->vad.reset();
        return AAUDIO_CALLBACK_RESULT_CONTINUE;
    }

    if (ctx->vadBuffer.size() < ctx->VAD_FRAME_SIZE) return AAUDIO_CALLBACK_RESULT_CONTINUE;

    while (true) {
        int16_t frame[512];
        {
            std::lock_guard<std::mutex> lock(ctx->bufferMutex);
            if (ctx->vadBuffer.size() < ctx->VAD_FRAME_SIZE) break;
            memcpy(frame, ctx->vadBuffer.data(), 512 * sizeof(int16_t));
        }

        VAD::Result res = ctx->vad.processFrame(frame, ctx->VAD_FRAME_SIZE);
        
        struct timespec ts;
        clock_gettime(CLOCK_MONOTONIC, &ts);
        int64_t nowMs = (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;

        if (nowMs - ctx->lastVolumeReportTime >= 20) {
            std::lock_guard<std::mutex> lock(ctx->queueMutex);
            AudioEngineContext::JniCommand cmd;
            cmd.type = AudioEngineContext::CMD_VOLUME_CHANGE;
            cmd.volume = res.rms;
            ctx->jniQueue.push(std::move(cmd));
            ctx->queueCv.notify_one();
            ctx->lastVolumeReportTime = nowMs;
        }

        if (res.stateChanged) {
            LOGD("VAD: State Changed to %d (RMS: %.2f)", (int)res.state, res.rms);
            if (res.state == VAD::State::SPEAKING) {
                ctx->isStreamActive = true;
                ctx->hasTriggeredSpeechStart = false;
            } else if (res.state == VAD::State::SILENCE_DETECTED) {
                LOGD("VAD: Silence detected, ending stream");
                ctx->isStreamActive = false;
                {
                    std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
                    if (!ctx->accumulatedChunk.empty()) {
                        if (ctx->hasTriggeredSpeechStart) {
                            std::lock_guard<std::mutex> qLock(ctx->queueMutex);
                            AudioEngineContext::JniCommand cmd;
                            cmd.type = AudioEngineContext::CMD_SPEECH_END;
                            cmd.audioData = std::move(ctx->accumulatedChunk);
                            ctx->jniQueue.push(std::move(cmd));
                            ctx->queueCv.notify_one();
                        } else {
                            LOGD("VAD: Short utterance rejected (< 1s)");
                        }
                        ctx->accumulatedChunk.clear();
                    }
                }
            }
        }

        if (ctx->isStreamActive) {
            if (res.isLoud) ctx->consecutiveSilenceFrames = 0;
            else ctx->consecutiveSilenceFrames++;

            if (ctx->consecutiveSilenceFrames <= 65) { // Keep up to 65 frames of silence (~2s)
                std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
                ctx->accumulatedChunk.insert(ctx->accumulatedChunk.end(), frame, frame + ctx->VAD_FRAME_SIZE);
                
                if (!ctx->hasTriggeredSpeechStart && ctx->accumulatedChunk.size() >= ctx->CONFIRMATION_SIZE) {
                    ctx->hasTriggeredSpeechStart = true;
                    std::lock_guard<std::mutex> qLock(ctx->queueMutex);
                    AudioEngineContext::JniCommand cmdStart;
                    cmdStart.type = AudioEngineContext::CMD_SPEECH_START;
                    ctx->jniQueue.push(std::move(cmdStart));
                    
                    if (!ctx->preRollBuffer.empty()) {
                        AudioEngineContext::JniCommand chunkCmd;
                        chunkCmd.type = AudioEngineContext::CMD_STREAMING_CHUNK;
                        chunkCmd.audioData = std::move(ctx->preRollBuffer);
                        chunkCmd.chunkType = 0x01;
                        ctx->jniQueue.push(std::move(chunkCmd));
                        ctx->preRollBuffer.clear();
                    }
                    
                    AudioEngineContext::JniCommand chunkCmd2;
                    chunkCmd2.type = AudioEngineContext::CMD_STREAMING_CHUNK;
                    chunkCmd2.audioData = ctx->accumulatedChunk;
                    chunkCmd2.chunkType = 0x01;
                    ctx->jniQueue.push(std::move(chunkCmd2));
                    
                    ctx->queueCv.notify_one();
                    ctx->accumulatedChunk.clear();
                    
                } else if (ctx->hasTriggeredSpeechStart && ctx->accumulatedChunk.size() >= ctx->CHUNK_SIZE_LIMIT) {
                    std::lock_guard<std::mutex> qLock(ctx->queueMutex);
                    AudioEngineContext::JniCommand cmd;
                    cmd.type = AudioEngineContext::CMD_STREAMING_CHUNK;
                    cmd.audioData = ctx->accumulatedChunk;
                    cmd.chunkType = 0x01;
                    ctx->jniQueue.push(std::move(cmd));
                    ctx->queueCv.notify_one();
                    ctx->accumulatedChunk.clear();
                }
            }
        } else {
            std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
            ctx->preRollBuffer.insert(ctx->preRollBuffer.end(), frame, frame + ctx->VAD_FRAME_SIZE);
            if (ctx->preRollBuffer.size() > ctx->PRE_ROLL_LIMIT) {
                ctx->preRollBuffer.erase(ctx->preRollBuffer.begin(), ctx->preRollBuffer.begin() + (ctx->preRollBuffer.size() - ctx->PRE_ROLL_LIMIT));
            }
        }

        {
            std::lock_guard<std::mutex> lock(ctx->bufferMutex);
            if (!ctx->vadBuffer.empty()) {
                ctx->vadBuffer.erase(ctx->vadBuffer.begin(), ctx->vadBuffer.begin() + ctx->VAD_FRAME_SIZE);
            }
        }
    }
    
    static int stateLogCount = 0;
    if (stateLogCount++ % 500 == 0) {
        aaudio_stream_state_t state = AAudioStream_getState(stream);
        if (state != AAUDIO_STREAM_STATE_STARTED) {
            LOGW("AAudio Stream State: %d", (int)state);
        }
    }

    return AAUDIO_CALLBACK_RESULT_CONTINUE;
}

// --- JNI Exports ---

extern "C" JNIEXPORT jlong JNICALL
Java_com_jakbox_speax_NativeAudioEngine_create(JNIEnv* env, jobject obj) {
    LOGD("NativeAudioEngine: Created");
    return (jlong)new AudioEngineContext(env, obj);
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_destroy(JNIEnv* env, jobject obj, jlong ptr) {
    LOGD("NativeAudioEngine: Destroyed");
    delete (AudioEngineContext*)ptr;
}

extern "C" JNIEXPORT jint JNICALL
Java_com_jakbox_speax_NativeAudioEngine_startRecording(JNIEnv* env, jobject obj, jlong ptr) {
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    std::lock_guard<std::mutex> lock(ctx->engineMutex);
    LOGD("NativeAudioEngine: Starting recording");

    if (ctx->stream) {
        LOGW("NativeAudioEngine: Stream already exists, stopping and cleaning up old stream.");
        AAudioStream_requestStop(ctx->stream);
        AAudioStream_close(ctx->stream);
        ctx->stream = nullptr;
    }

    {
        std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
        ctx->vad.reset();
        ctx->isStreamActive = false;
        ctx->hasTriggeredSpeechStart = false;
        ctx->accumulatedChunk.clear();
        ctx->preRollBuffer.clear();
        ctx->vadBuffer.clear();
    }

    AAudioStreamBuilder* builder;
    AAudio_createStreamBuilder(&builder);
    AAudioStreamBuilder_setSampleRate(builder, ctx->sampleRate);
    AAudioStreamBuilder_setChannelCount(builder, 1);
    AAudioStreamBuilder_setFormat(builder, AAUDIO_FORMAT_PCM_I16);
    AAudioStreamBuilder_setDirection(builder, AAUDIO_DIRECTION_INPUT);
#if __ANDROID_API__ >= 28
    AAudioStreamBuilder_setUsage(builder, AAUDIO_USAGE_VOICE_COMMUNICATION);
    AAudioStreamBuilder_setContentType(builder, AAUDIO_CONTENT_TYPE_SPEECH);
    
    std::string profile = ctx->vad.getMicProfile();
    if (profile == "sensitive") {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_UNPROCESSED);
    } else if (profile == "heavy") {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_VOICE_COMMUNICATION);
    } else {
        AAudioStreamBuilder_setInputPreset(builder, AAUDIO_INPUT_PRESET_VOICE_RECOGNITION);
    }
#endif
    AAudioStreamBuilder_setPerformanceMode(builder, AAUDIO_PERFORMANCE_MODE_LOW_LATENCY);
    AAudioStreamBuilder_setSharingMode(builder, AAUDIO_SHARING_MODE_SHARED);
    AAudioStreamBuilder_setDataCallback(builder, dataCallback, ctx);
    AAudioStreamBuilder_setErrorCallback(builder, errorCallback, ctx);

    aaudio_result_t result = AAudioStreamBuilder_openStream(builder, &ctx->stream);
    AAudioStreamBuilder_delete(builder);

    if (result != AAUDIO_OK) {
        LOGE("Failed to open AAudio stream: %s", AAudio_convertResultToText(result));
        return (jint)result;
    }

    result = AAudioStream_requestStart(ctx->stream);
    if (result != AAUDIO_OK) {
        LOGE("Failed to start AAudio stream: %s", AAudio_convertResultToText(result));
        AAudioStream_close(ctx->stream);
        ctx->stream = nullptr;
        return (jint)result;
    }

    LOGD("NativeAudioEngine: Recording started successfully");
    return (jint)AAUDIO_OK;
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_stopRecording(JNIEnv* env, jobject obj, jlong ptr) {
    LOGD("NativeAudioEngine: stopRecording called");
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    std::lock_guard<std::mutex> lock(ctx->engineMutex);
    if (ctx->stream) {
        AAudioStream_requestStop(ctx->stream);
        AAudioStream_close(ctx->stream);
        ctx->stream = nullptr;
    }
    
    std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
    ctx->vad.reset();
    ctx->isStreamActive = false;
    ctx->hasTriggeredSpeechStart = false;
    ctx->accumulatedChunk.clear();
    ctx->preRollBuffer.clear();
    ctx->vadBuffer.clear();
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_forceEndStreaming(JNIEnv* env, jobject obj, jlong ptr) {
    LOGD("NativeAudioEngine: forceEndStreaming called");
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    std::lock_guard<std::mutex> lock(ctx->engineMutex);
    
    if (ctx->isStreamActive && !ctx->accumulatedChunk.empty()) {
        std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
        if (ctx->hasTriggeredSpeechStart) {
            std::lock_guard<std::mutex> qLock(ctx->queueMutex);
            AudioEngineContext::JniCommand cmd;
            cmd.type = AudioEngineContext::CMD_SPEECH_END;
            cmd.audioData = std::move(ctx->accumulatedChunk);
            ctx->jniQueue.push(std::move(cmd));
            ctx->queueCv.notify_one();
        }
        ctx->accumulatedChunk.clear();
    }
    ctx->isStreamActive = false;
    ctx->hasTriggeredSpeechStart = false;
    
    std::lock_guard<std::mutex> bLock(ctx->bufferMutex);
    ctx->vad.reset();
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_setMicProfile(JNIEnv* env, jobject obj, jlong ptr, jstring profile) {
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    const char* nativeProfile = env->GetStringUTFChars(profile, nullptr);
    ctx->vad.setMicProfile(nativeProfile);
    env->ReleaseStringUTFChars(profile, nativeProfile);
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_setMuted(JNIEnv* env, jobject obj, jlong ptr, jboolean muted) {
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    ctx->isMuted = muted;
    if (muted) {
        LOGD("NativeAudioEngine: Muted");
    } else {
        LOGD("NativeAudioEngine: Unmuted");
    }
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_setThreshold(JNIEnv* env, jobject obj, jlong ptr, jdouble threshold) {
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    ctx->vad.setThreshold(threshold);
}

extern "C" JNIEXPORT void JNICALL
Java_com_jakbox_speax_NativeAudioEngine_setPlaybackActive(JNIEnv* env, jobject obj, jlong ptr, jboolean active) {
    AudioEngineContext* ctx = (AudioEngineContext*)ptr;
    ctx->vad.setPlaybackActive(active);
}
