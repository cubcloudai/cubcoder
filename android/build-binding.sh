#!/bin/bash
# Rebuild the gomobile Android binding (native libgojni.so + Java classes).
#
# CRITICAL: the .so must be linked with 16 KB ELF segment alignment, or the app
# crashes on launch with UnsatisfiedLinkError ("dlopen ... not 16 KB aligned")
# on 16 KB-page devices (Android 15+, recent Pixels). NDK r27 still defaults to
# 4 KB; r28+ defaults to 16 KB. We force it via -extldflags regardless.
#
# Outputs are consumed by build-apk.sh:
#   build/native/<abi>/libgojni.so   (16 KB-aligned native libs)
#   bindings/java/gomobile-bindings.jar
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)/.."   # repo root

export ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}"
export ANDROID_HOME="$ANDROID_SDK_ROOT"

# NDK: the dir containing source.properties + toolchains.
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
    for d in "$ANDROID_SDK_ROOT"/ndk/*/ "$ANDROID_SDK_ROOT"/ndk-bundle/; do
        [ -f "$d/source.properties" ] && [ -d "$d/toolchains" ] && ANDROID_NDK_HOME="${d%/}" && break
    done
fi
export ANDROID_NDK_HOME

# JDK for gomobile's javac step (Android Studio JBR ships one).
if [ -z "${JAVA_HOME:-}" ] || [ ! -x "${JAVA_HOME:-}/bin/javac" ]; then
    for J in /usr/lib/jvm/*/bin/javac /snap/android-studio/*/jbr/bin/javac; do
        [ -x "$J" ] && JAVA_HOME="$(dirname "$(dirname "$J")")" && break
    done
fi
export JAVA_HOME

# go.mod requires go 1.26; the system `go` may be an older base that would try
# (and, offline, fail) to fetch the 1.26 toolchain. Prefer the real 1.26 binary
# if it's already in the module cache, and pin GOTOOLCHAIN=local so no download
# is attempted.
# Prefer the newest cached go toolchain (go.mod needs >= 1.26); the system `go`
# may be an older base that can't parse the `tool` directive or the go version.
GO_BIN="go"
newest="$(ls -d "$HOME"/go/pkg/mod/golang.org/toolchain@*/bin/go 2>/dev/null | sort -V | tail -1)"
[ -n "$newest" ] && [ -x "$newest" ] && GO_BIN="$newest"
export PATH="$(dirname "$GO_BIN"):$HOME/go/bin:$JAVA_HOME/bin:$PATH"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

echo "NDK:  $ANDROID_NDK_HOME"
echo "JDK:  $JAVA_HOME"
echo "go:   $($GO_BIN version)"
echo "javac:$(command -v javac)"
echo ""

# 16 KB ELF alignment. Both -z options in ONE comma-separated -extldflags token
# (a space would split it and send the tail to the Go linker, not the C linker).
LDFLAGS="-extldflags=-Wl,-z,max-page-size=16384,-z,common-page-size=16384"

echo "Running gomobile bind (a few minutes)..."
gomobile bind \
    -target=android -androidapi=26 \
    "-ldflags=$LDFLAGS" \
    -o android/cubcoder.aar ./gomobile

# Unpack the .aar into the layout build-apk.sh expects.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
unzip -q android/cubcoder.aar -d "$WORK"

for abi in armeabi-v7a arm64-v8a x86 x86_64; do
    mkdir -p "android/build/native/$abi"
    cp "$WORK/jni/$abi/libgojni.so" "android/build/native/$abi/libgojni.so"
done
mkdir -p android/bindings/java
cp "$WORK/classes.jar" android/bindings/java/gomobile-bindings.jar

echo ""
echo "=== verify 16 KB ELF alignment (want 0x4000) ==="
READELF="$(command -v readelf || true)"
if [ -n "$READELF" ]; then
    for abi in arm64-v8a armeabi-v7a x86 x86_64; do
        printf "  %-12s %s\n" "$abi" \
            "$("$READELF" -lW "android/build/native/$abi/libgojni.so" | awk '/LOAD/{print $NF}' | sort -u | tr '\n' ' ')"
    done
fi

echo ""
echo "Binding rebuilt. Now run: android/build-apk.sh"
