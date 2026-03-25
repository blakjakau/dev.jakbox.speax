plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.jakbox.speax"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.jakbox.speax"
        minSdk = 26 // Android 8.0 minimum (good for Audio APIs)
        targetSdk = 34
        versionCode = 1
        versionName = "1.0"
        manifestPlaceholders["androidx.startup.InitializationProvider"] = "androidx.startup.InitializationProvider"
    }

    buildFeatures {
        compose = true
    }
    composeOptions {
        kotlinCompilerExtensionVersion = "1.5.9"
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    
    externalNativeBuild {
        cmake {
            path = file("src/main/cpp/CMakeLists.txt")
            version = "3.22.1"
        }
    }

    packaging {
        jniLibs {
            useLegacyPackaging = true
        }
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.12.0")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.7.0")
    implementation("androidx.lifecycle:lifecycle-viewmodel-ktx:2.7.0")
    implementation("androidx.savedstate:savedstate-ktx:1.2.1")
    implementation("androidx.activity:activity-compose:1.8.2")
    implementation(platform("androidx.compose:compose-bom:2024.01.00"))
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("androidx.browser:browser:1.8.0")
    
    // The mighty OkHttp for our WebSocket
    implementation("com.squareup.okhttp3:okhttp:4.12.0")

    // Sherpa-ONNX for native in-memory Piper TTS
    implementation(files("libs/sherpa-onnx-1.12.29.aar"))
}
