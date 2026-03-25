package com.jakbox.speax

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.view.KeyEvent

class MediaButtonReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (Intent.ACTION_MEDIA_BUTTON != intent.action) return
        
        val keyEvent = intent.getParcelableExtra<KeyEvent>(Intent.EXTRA_KEY_EVENT) ?: return
        
        // Only trigger on the button release to prevent double-firing
        if (keyEvent.action == KeyEvent.ACTION_UP) {
            val localIntent = Intent("SPEAX_HARDWARE_BTN").apply {
                setPackage(context.packageName)
                putExtra("keycode", keyEvent.keyCode)
            }
            // Fire an internal broadcast that MainActivity can catch
            context.sendBroadcast(localIntent) 
        }
    }
}