#include "bridge.h"
#include "piper.hpp"
#include <string>
#include <vector>
#include <stdlib.h>
#include <iostream>

struct RealContext {
    piper::PiperConfig config;
    piper::Voice voice;
    bool voice_loaded = false;
};

// Create a new empty context
PiperContext piper_init_context() {
    return (PiperContext)new RealContext();
}

// Load a specific voice into this context
int piper_load_voice(PiperContext ctx, const char* model_path, const char* config_path, const char* espeak_data_path) {
    if (!ctx || !model_path || !config_path || !espeak_data_path) return -1;
    RealContext* c = reinterpret_cast<RealContext*>(ctx);
    
    // Set espeak path for synthesis calls (though espeak is global, piper logic uses it)
    c->config.eSpeakDataPath = std::string(espeak_data_path);
    c->config.useESpeak = true;
    
    std::optional<piper::SpeakerId> speakerId;
    try {
        piper::loadVoice(c->config, std::string(model_path), std::string(config_path), c->voice, speakerId, false);
        c->voice_loaded = true;
        return 0; // Success
    } catch (const std::exception& e) {
        std::cerr << "Piper Error loading voice: " << e.what() << std::endl;
        return -1;
    } catch (...) {
        return -1;
    }
}

// Synthesize text to PCM
int piper_synthesize(PiperContext ctx, const char* text, int16_t** out_buffer, float length_scale, float noise_scale, float noise_w) {
    if (!ctx || !text || !out_buffer) return -1;
    RealContext* c = reinterpret_cast<RealContext*>(ctx);
    if (!c->voice_loaded) return -1;

    // Apply custom parameters
    c->voice.synthesisConfig.lengthScale = length_scale;
    c->voice.synthesisConfig.noiseScale = noise_scale;
    c->voice.synthesisConfig.noiseW = noise_w;

    std::vector<int16_t> audioBuffer;
    piper::SynthesisResult result;
    
    try {
        piper::textToAudio(c->config, c->voice, std::string(text), audioBuffer, result, nullptr);
    } catch (const std::exception& e) {
        std::cerr << "Piper Error during synthesis: " << e.what() << std::endl;
        return -1;
    } catch (...) {
        return -1;
    }
    
    int size = audioBuffer.size();
    std::cerr << "[Bridge] Synthesize: generated " << size << " samples" << std::endl;
    if (size == 0) {
        *out_buffer = nullptr;
        return 0;
    }
    
    *out_buffer = (int16_t*)malloc(size * sizeof(int16_t));
    if (!*out_buffer) return -1;
    
    // Copy data
    for (int i=0; i<size; i++){
        (*out_buffer)[i] = audioBuffer[i];
    }
    return size;
}

int piper_synthesize_stream(PiperContext ctx, const char* text, PiperAudioCallback callback, void* userdata, float length_scale, float noise_scale, float noise_w) {
    if (!ctx || !text || !callback) return -1;
    RealContext* c = reinterpret_cast<RealContext*>(ctx);
    if (!c->voice_loaded) return -1;

    // Apply custom parameters
    c->voice.synthesisConfig.lengthScale = length_scale;
    c->voice.synthesisConfig.noiseScale = noise_scale;
    c->voice.synthesisConfig.noiseW = noise_w;

    std::vector<int16_t> audioBuffer;
    piper::SynthesisResult result;

    size_t lastSize = 0;
    // In this version of Piper, the callback has no arguments. 
    // We must check the growth of audioBuffer manually.
    std::function<void()> audioCallback = [&]() {
        if (audioBuffer.size() > lastSize) {
            size_t diff = audioBuffer.size() - lastSize;
            std::cerr << "[Bridge] Callback: sending " << diff << " samples (" << audioBuffer.size() << " total)" << std::endl;
            callback(&audioBuffer[lastSize], (int)diff, userdata);
            lastSize = audioBuffer.size();
        }
    };

    try {
        piper::textToAudio(c->config, c->voice, std::string(text), audioBuffer, result, audioCallback);
        // FINAL FLUSH: ensure any remaining data at the end of the buffer is sent.
        if (audioBuffer.size() > lastSize) {
            size_t diff = audioBuffer.size() - lastSize;
            std::cerr << "[Bridge] Final Flush: sending " << diff << " samples (" << audioBuffer.size() << " total)" << std::endl;
            callback(&audioBuffer[lastSize], (int)diff, userdata);
        }
    } catch (const std::exception& e) {
        std::cerr << "Piper Error during streaming synthesis: " << e.what() << std::endl;
        return -1;
    } catch (...) {
        return -1;
    }
    
    return 0;
}

void piper_free_buffer(int16_t* buffer) {
    if (buffer) {
        free(buffer);
    }
}

void piper_free_context(PiperContext ctx) {
    if (ctx) {
        RealContext* c = reinterpret_cast<RealContext*>(ctx);
        delete c;
    }
}

// Track global config for initialization and termination
static piper::PiperConfig global_config;

void piper_initialize(const char* espeak_data_path) {
    if (espeak_data_path) {
        global_config.eSpeakDataPath = std::string(espeak_data_path);
    }
    global_config.useESpeak = true;
    piper::initialize(global_config);
}

void piper_terminate() {
    piper::terminate(global_config);
}
