# Speax: Low-Latency AI Voice router

Speax is a local-first, privacy-conscious AI implementation. Designed for low-latency, real-time interaction, it aims to bridge the gap between powerful online LLMs and personal hardware with a focus on data sovereignty and conversational fluidity.

## Core Features
*   **Low-Latency Pipeline:** Real-time bi-directional streaming via WebSockets with optimized chunking for gapless TTS.
*   **Intelligent Barge-in:** Fully asynchronous audio processing allowing the AI to be interrupted instantly when the user speaks.
*   **Tiered Memory System:** Decoupled UI and LLM history with background auto-summarization to maintain context across 100+ turns.
*   **Multi-Engine Routing:** First-class support for local LLMs (Ollama) and cloud APIs (Gemini) with per-provider token tracking.
*   **Cross-Platform Sync:** Unified session management via Google OAuth2, ensuring the same "Alyx" persona and history on both Web and Native Android.
*   **Hardware-First Android:** Native Kotlin implementation using `AudioRecord` and `SpeechRecognizer` for hardware-level noise suppression and reduced bandwidth.

## Tech Stack
*   **Backend:** Go (Golang)
*   **Web Frontend:** Vanilla HTML/CSS/JavaScript (No frameworks)
*   **Android Client:** Native Kotlin
*   **STT:** Whisper.cpp (Local) routing or Native Android / Web Recognition APIs
*   **LLM:** Ollama (Local) or Google Gemini (Cloud) - with user-provided API keys
*   **TTS:** Piper TTS (Local) routing for audio synthesis

## Quick Start
1.  **Server:** Ensure you have Go installed. Place your `google-client-secret.json` in the root. Run `go run server.go`.
2.  **Web:** Serve the `./public` directory. Access on your local network (e.g., `https://<ip>:3000`).
3.  **Android:** Open the `/android` directory in Android Studio and build to your device (or old-school CLI build, IDE's suck all the fun out of it)

## Architecture Philosophy
Speax is built on a "simple-is-better" approach. By keeping the core communications protocol (WebSockets + PCM audio) platform-agnostic, we maintain high compatibility and low overhead. Where low cost, self-hosted or free integrations are possible, they should be supported as first-class options.

---
*Built by jakbox.dev*
