#!/bin/bash
# Reproducible APK build for cubcoder Android.
#
# Reuses the gomobile output (native libgojni.so libs + Java bindings jar), which
# come from `gomobile bind` (the slow NDK step). Everything else — resources,
# manifest, MainActivity, dexing, packaging, signing — is rebuilt from source
# here so the APK is reproducible and carries a valid versionCode/versionName.
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

# --- toolchain -------------------------------------------------------------
export ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}"

# Pick a JDK (Android Studio's bundled JBR ships javac).
if [ -z "${JAVA_HOME:-}" ] || [ ! -x "${JAVA_HOME:-}/bin/javac" ]; then
    for J in /usr/lib/jvm/*/bin/javac /snap/android-studio/*/jbr/bin/javac; do
        [ -x "$J" ] && JAVA_HOME="$(dirname "$(dirname "$J")")" && break
    done
fi
export JAVA_HOME
export PATH="$JAVA_HOME/bin:$PATH"

BT="$(ls -d "$ANDROID_SDK_ROOT"/build-tools/* | sort -V | tail -1)"
AAPT2="$BT/aapt2"
D8="$BT/d8"
ZIPALIGN="$BT/zipalign"
APKSIGNER="$BT/apksigner"
ANDROID_JAR="$(ls -d "$ANDROID_SDK_ROOT"/platforms/android-* | sort -V | tail -1)/android.jar"

echo "SDK:        $ANDROID_SDK_ROOT"
echo "build-tools:$BT"
echo "JDK:        $JAVA_HOME"
echo "android.jar:$ANDROID_JAR"
echo ""

BINDINGS_JAR="bindings/java/gomobile-bindings.jar"
NATIVE_DIR="build/native"          # build/native/<abi>/libgojni.so from gomobile bind
for req in "$AAPT2" "$D8" "$ZIPALIGN" "$APKSIGNER" "$ANDROID_JAR" "$BINDINGS_JAR"; do
    [ -e "$req" ] || { echo "ERROR: missing required input: $req" >&2; exit 1; }
done
ls "$NATIVE_DIR"/*/libgojni.so >/dev/null || { echo "ERROR: no native libs in $NATIVE_DIR" >&2; exit 1; }

# --- clean build dir -------------------------------------------------------
OUT="out"
rm -rf "$OUT"
mkdir -p "$OUT/res-flat" "$OUT/gen" "$OUT/classes" "$OUT/apk"

# --- 1. compile + link resources ------------------------------------------
echo "[1/5] aapt2 compile resources"
"$AAPT2" compile --dir res -o "$OUT/res-flat"

echo "[2/5] aapt2 link (manifest carries versionCode/versionName)"
"$AAPT2" link \
    -o "$OUT/base.apk" \
    --manifest AndroidManifest.xml \
    -I "$ANDROID_JAR" \
    -A assets \
    --java "$OUT/gen" \
    --min-sdk-version 26 \
    --target-sdk-version 34 \
    "$OUT"/res-flat/*.flat

# --- 2. compile Java (MainActivity + generated R) --------------------------
echo "[3/5] javac"
find src "$OUT/gen" -name '*.java' > "$OUT/sources.txt"
javac -source 8 -target 8 \
    -Xlint:-options \
    -classpath "$ANDROID_JAR:$BINDINGS_JAR" \
    -d "$OUT/classes" \
    @"$OUT/sources.txt"

# --- 3. dex (app classes + gomobile binding classes) -----------------------
echo "[4/5] d8 -> classes.dex"
APP_CLASSES=$(find "$OUT/classes" -name '*.class')
"$D8" --min-api 26 --lib "$ANDROID_JAR" \
    --output "$OUT" \
    $APP_CLASSES "$BINDINGS_JAR"

# --- 4. assemble, align, sign ---------------------------------------------
echo "[5/5] package + sign"
# base.apk from aapt2 holds AndroidManifest + resources.arsc + res/.
( cd "$OUT/apk" && unzip -q ../base.apk )
cp "$OUT/classes.dex" "$OUT/apk/classes.dex"
for abi in armeabi-v7a arm64-v8a x86 x86_64; do
    so="$NATIVE_DIR/$abi/libgojni.so"
    [ -f "$so" ] || { echo "ERROR: missing $so" >&2; exit 1; }
    mkdir -p "$OUT/apk/lib/$abi"
    cp "$so" "$OUT/apk/lib/$abi/libgojni.so"
done

# Zip entry compression matters for install:
#   - resources.arsc MUST be stored uncompressed + 4-byte aligned for
#     targetSdk >= 30, or the installer rejects the APK ("App not installed").
#   - .so stored uncompressed (page-aligned) so they load regardless of
#     extractNativeLibs.
# Everything else is compressed.
rm -f "$OUT/unsigned.apk"
( cd "$OUT/apk" && \
    zip -q -X -0 ../unsigned.apk resources.arsc $(find lib -name '*.so') && \
    zip -q -X -r ../unsigned.apk . -x 'lib/*' -x resources.arsc )

# -P 16: align .so entries to 16 KB so they load on 16 KB-page devices
# (Android 15+/Pixel etc). The libs themselves must also be linked with
# 16 KB ELF segment alignment — see build-binding.sh.
"$ZIPALIGN" -P 16 -f 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"

KEYSTORE="debug.keystore"
if [ ! -f "$KEYSTORE" ]; then
    keytool -genkeypair -v -keystore "$KEYSTORE" -storepass android -alias debug \
        -keypass android -keyalg RSA -keysize 2048 -validity 10000 \
        -dname "CN=Android Debug,O=CubCoder,C=US"
fi

"$APKSIGNER" sign \
    --ks "$KEYSTORE" --ks-key-alias debug \
    --ks-pass pass:android --key-pass pass:android \
    --v1-signing-enabled true --v2-signing-enabled true --v3-signing-enabled true \
    --out cubcoder-debug.apk \
    "$OUT/aligned.apk"

echo ""
echo "=== verify ==="
"$APKSIGNER" verify --print-certs cubcoder-debug.apk >/dev/null && echo "signature OK"
"$BT/aapt" dump badging cubcoder-debug.apk 2>/dev/null | grep -E "^package:"

echo ""
echo "Built: android/cubcoder-debug.apk"
echo "Install with: adb install -r android/cubcoder-debug.apk"
