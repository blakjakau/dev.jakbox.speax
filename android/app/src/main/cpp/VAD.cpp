#include "VAD.h"
#if defined(__ARM_NEON) || defined(__ARM_NEON__)
#include <arm_neon.h>
#endif
#include <android/log.h>

#define LOG_TAG "VAD-Native"
#define LOGD(...) __android_log_print(ANDROID_LOG_DEBUG, LOG_TAG, __VA_ARGS__)

VAD::VAD(int sampleRate, int frameSizeSamples) 
    : sampleRate(sampleRate), frameSizeSamples(frameSizeSamples) {}

VAD::Result VAD::processFrame(const int16_t* pcm, int sizeSamples) {
    std::lock_guard<std::mutex> lock(vadMutex);
    float rms = calculateRMS(pcm, sizeSamples);
    
    // Log occasionally to avoid spamming but still show activity
    static int frameCount = 0;
    if (frameCount++ % 30 == 0) {
        LOGD("VAD: Signal RMS: %.2f (Threshold: %.2f, Samples: %d)", rms, noiseThreshold.load(), sizeSamples);
    }

    double currentThreshold = (profile == "adaptive") ? std::max(100.0, averageSpeechRms * 0.3) : (profile == "standard" ? noiseThreshold.load() * 1.15 : noiseThreshold.load());
    if (profile == "sensitive") currentThreshold = 150.0;

    // Boost threshold during playback for HEAVY and ADAPTIVE to reduce false barge-ins
    if (isPlaybackActive && (profile == "heavy" || profile == "adaptive")) {
        currentThreshold *= 1.5;
    }

    State nextState = currentState;
    bool stateChanged = false;

    bool isLoud = (rms > currentThreshold);
    if (isLoud) {
        if (!isSpeaking) {
            isSpeaking = true;
            nextState = State::SPEAKING;
            stateChanged = true;
            // Note: Pre-roll handling will be done in NativeAudioEngine to manage memory efficiently
        }
        silenceFrames = 0;
    } else if (isSpeaking) {
        silenceFrames++;
        if (silenceFrames >= SILENCE_FRAMES_LIMIT) {
            isSpeaking = false;
            silenceFrames = 0;
            nextState = State::SILENCE_DETECTED;
            stateChanged = true;
        }
    }

    if (nextState == State::SILENCE_DETECTED && !stateChanged) {
        // Transition back to SILENCE after one frame of SILENCE_DETECTED
        nextState = State::SILENCE;
    }

    Result res = { nextState, rms, stateChanged, isLoud };
    currentState = nextState;
    return res;
}

float VAD::calculateRMS(const int16_t* pcm, int size) {
    if (size <= 0) return 0.0f;

    long long sumSquares = 0;
    int i = 0;

#if defined(__ARM_NEON) || defined(__ARM_NEON__)
    // SIMD Optimization using NEON
    int32x4_t sum_vec = vdupq_n_s32(0);
    
    // Process 8 samples at a time
    for (; i <= size - 8; i += 8) {
        int16x8_t samples = vld1q_s16(pcm + i);
        
        int16x4_t lo = vget_low_s16(samples);
        int16x4_t hi = vget_high_s16(samples);
        
        sum_vec = vmlal_s16(sum_vec, lo, lo);
        sum_vec = vmlal_s16(sum_vec, hi, hi);
        
        // Horizontal sum at every iteration (8 samples) to prevent int32 overflow
        // Max sum in one lane = 2 * 32768^2 = 2 * 10^9 (fits in int32)
        sumSquares += (long long)vgetq_lane_s32(sum_vec, 0) + vgetq_lane_s32(sum_vec, 1) +
                      vgetq_lane_s32(sum_vec, 2) + vgetq_lane_s32(sum_vec, 3);
        sum_vec = vdupq_n_s32(0);
    }
    
    // Horizontal sum of the remaining partial sums
    sumSquares += (long long)vgetq_lane_s32(sum_vec, 0) + vgetq_lane_s32(sum_vec, 1) +
                  vgetq_lane_s32(sum_vec, 2) + vgetq_lane_s32(sum_vec, 3);
#endif

    // Process remaining samples
    for (; i < size; ++i) {
        sumSquares += (long long)pcm[i] * pcm[i];
    }

    return std::sqrt((float)sumSquares / size);
}

void VAD::reset() {
    isSpeaking = false;
    silenceFrames = 0;
    currentState = State::SILENCE;
}
