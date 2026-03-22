cmake_minimum_required(VERSION 3.22.1)
project("native-audio")

# Add the native library
add_library(
    native-audio
    SHARED
    NativeAudioEngine.cpp
    VAD.cpp
)

# Enable NEON SIMD optimizations
if(ANDROID_ABI STREQUAL "armeabi-v7a")
    set_target_properties(native-audio PROPERTIES COMPILE_FLAGS "-mfpu=neon")
elseif(ANDROID_ABI STREQUAL "arm64-v8a")
    # NEON is mandatory in ARM64-v8a
endif()

# Link required libraries
target_link_libraries(
    native-audio
    aaudio
    log
    android
)
