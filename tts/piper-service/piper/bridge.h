#ifndef PIPER_BRIDGE_H
#define PIPER_BRIDGE_H
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif

typedef void* PiperContext;

// Create a new empty context
PiperContext piper_init_context();

// Load a specific voice into this context
int piper_load_voice(PiperContext ctx, const char* model_path, const char* config_path, const char* espeak_data_path);

typedef void (*PiperAudioCallback)(int16_t* data, int length, void* userdata);

// Synthesize text to PCM. Returns number of samples, or -1 on error. 
// Uses malloc to allocate out_buffer, caller must free using piper_free_buffer.
int piper_synthesize(PiperContext ctx, const char* text, int16_t** out_buffer, float length_scale, float noise_scale, float noise_w);

// Stream synthesis: calls callback for each audio chunk generated.
int piper_synthesize_stream(PiperContext ctx, const char* text, PiperAudioCallback callback, void* userdata, float length_scale, float noise_scale, float noise_w);

void piper_free_buffer(int16_t* buffer);
void piper_free_context(PiperContext ctx);

// Global piper/espeak initialization
void piper_initialize(const char* espeak_data_path);
void piper_terminate();

#ifdef __cplusplus
}
#endif
#endif
