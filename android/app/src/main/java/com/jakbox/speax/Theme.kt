package com.jakbox.speax

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

val DarkColors = darkColorScheme(
    background = Color(0xFF030B17),      // Slate Deep (Depressed/Shadows)
    surface = Color(0xFF0B1E36),         // Slate Mid (Cards/Surfaces)
    surfaceVariant = Color(0xFF152E4D),  // Slate High (Active Elements/Borders)
    primary = Color(0xFF0E639C),
    secondary = Color(0xFF00D1C1),
    onBackground = Color.White,
    onSurface = Color.White,
    outline = Color(0xFF152E4D)
)

val LightColors = lightColorScheme(
    background = Color(0xFFF5F5F5),
    surface = Color(0xFFFFFFFF),
    surfaceVariant = Color(0xFFE0E0E0),
    primary = Color(0xFF0E639C),
    secondary = Color(0xFF007A5E), // Darker green for contrast on light mode
    onBackground = Color(0xFF1E1E1E),
    onSurface = Color(0xFF1E1E1E),
    outline = Color.LightGray
)

@Composable
fun SpeaxTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = if (darkTheme) DarkColors else LightColors,
        content = content
    )
}
