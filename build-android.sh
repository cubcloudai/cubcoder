#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

export ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}"
export PATH="$ANDROID_SDK_ROOT/build-tools/37.0.0:$ANDROID_SDK_ROOT/platform-tools:$PATH"

# Use system Java or bundled one
JAVA_HOME="${JAVA_HOME:-/usr/lib/jvm/default-java}"
if [ ! -x "$JAVA_HOME/bin/java" ]; then
    # Try Android Studio bundled JBR
    if [ -x "/snap/android-studio/241/jbr/bin/java" ]; then
        JAVA_HOME="/snap/android-studio/241/jbr"
    elif [ -x "$HOME/.antigravity/extensions/redhat.java-1.55.0-linux-x64/jre/21.0.11-linux-x86_64/bin/java" ]; then
        JAVA_HOME="$HOME/.antigravity/extensions/redhat.java-1.55.0-linux-x64/jre/21.0.11-linux-x86_64"
    fi
fi
export PATH="$JAVA_HOME/bin:$PATH"

GOMOBILE_BIN="${HOME}/go/bin/gomobile"

echo "=== cubcoder Android APK Builder ==="
echo "SDK: $ANDROID_SDK_ROOT"
echo "Java: $JAVA_HOME/bin/java"
echo "Gomobile: $GOMOBILE_BIN"
echo ""

# Step 1: Build the gomobile iOS library (for bind)
echo "[1/4] Building gomobile iOS library..."
$GOMOBILE_BIN init
if [ $? -ne 0 ]; then
    echo "Warning: gomobile init failed (non-fatal for Android)"
fi

# Step 2: Bind for Android
echo "[2/4] Binding gomobile package for Android..."
$GOMOBILE_BIN bind -target=android -androidapi=24 -o android/cubcoder.a ./gomobile
if [ $? -ne 0 ]; then
    echo "ERROR: gomobile bind failed"
    exit 1
fi
echo "✓ cubcoder.a generated"

# Step 3: Build the APK using aapt/d8
echo "[3/4] Building APK..."
mkdir -p android/obj android/abi/android/arm64-v8a android/abi/android/armeabi-v7a

# Copy .so files from gomobile output
if [ -d "cubcoder.android" ]; then
    cp cubcoder.android/*.so android/abi/android/arm64-v8a/ 2>/dev/null || true
fi

# Build APK with aapt
aapt package -f \
    -M android/AndroidManifest.xml \
    -S android/res \
    -A android/assets \
    -I "$ANDROID_SDK_ROOT/platforms/android-36/android.jar" \
    -F android/build-unsigned.apk \
    android/obj

if [ $? -ne 0 ]; then
    echo "ERROR: aapt package failed"
    exit 1
fi

# Step 4: Sign the APK
echo "[4/4] Signing APK..."
# Create a debug keystore if it doesn't exist
KEYSTORE_PATH="android/debug.keystore"
if [ ! -f "$KEYSTORE_PATH" ]; then
    keytool -genkey -v -keystore "$KEYSTORE_PATH" -storepass android -alias debug \
        -keypass android -keyalg RSA -keysize 2048 -validity 10000 \
        -dname "CN=Android Debug,O=Android,C=US" 2>/dev/null || true
fi

apksigner sign --ks "$KEYSTORE_PATH" --ks-key-alias debug --ks-pass pass:android --key-pass pass:android \
    android/build-unsigned.apk

mv android/build-unsigned.apk android/cubcoder-debug.apk

echo ""
echo "=== Build complete! ==="
echo "APK: android/cubcoder-debug.apk"
echo ""
echo "To install on a connected device:"
echo "  adb install -r android/cubcoder-debug.apk"
