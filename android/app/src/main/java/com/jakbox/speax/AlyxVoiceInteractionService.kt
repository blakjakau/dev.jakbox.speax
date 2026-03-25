package com.jakbox.speax

import android.service.voice.VoiceInteractionService

/**
 * Entry point for the Android Voice Interaction system.
 * This service is bound by the system when the user selects Speax as their default assistant.
 */
class AlyxVoiceInteractionService : VoiceInteractionService() {
    override fun onReady() {
        super.onReady()
        // Initialize persistent resources if needed
    }
}
