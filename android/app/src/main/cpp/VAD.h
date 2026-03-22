#ifndef VAD_H
#define VAD_H

#include <vector>
#include <cstdint>
#include <string>
#include <cmath>
#include <algorithm>
#include <atomic>
#include <mutex>

class VAD {
public:
    VAD(int sampleRate, int frameSizeSamples);
    
    enum class State {
        SILENCE,
        SPEECH_STARTING, // During pre-roll/initial detection
        SPEAKING,
        SILENCE_DETECTED // Just transitioned from speaking to silence
    };

    struct Result {
        State state;
        float rms;
        bool stateChanged;
        bool isLoud;
    };

    Result processFrame(const int16_t* pcm, int sizeSamples);
    
    void setThreshold(double threshold) { noiseThreshold.store(threshold); }
    void setMicProfile(const std::string& newProfile) { 
        std::lock_guard<std::mutex> lock(vadMutex);
        profile = newProfile; 
    }
    std::string getMicProfile() {
        std::lock_guard<std::mutex> lock(vadMutex);
        return profile;
    }
    void reset();

private:
    float calculateRMS(const int16_t* pcm, int size);
    
    int sampleRate;
    int frameSizeSamples;
    std::atomic<double> noiseThreshold{255.0};
    double averageSpeechRms = 600.0;
    bool isSpeaking = false;
    int silenceFrames = 0;
    const int SILENCE_FRAMES_LIMIT = 65;
    std::string profile = "standard";
    std::mutex vadMutex;
    
    State currentState = State::SILENCE;
};

#endif // VAD_H
