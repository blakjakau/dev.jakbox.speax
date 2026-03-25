package com.jakbox.speax

import android.os.Bundle
import android.service.voice.VoiceInteractionSession
import android.service.voice.VoiceInteractionSessionService

/**
 * Manages the lifecycle of VoiceInteractionSession instances.
 * This service is bound by the system to start a new voice interaction session.
 */
class AlyxVoiceInteractionSessionService : VoiceInteractionSessionService() {
    override fun onNewSession(args: Bundle?): VoiceInteractionSession {
        return AlyxVoiceInteractionSession(this)
    }
}
