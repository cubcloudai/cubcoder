LOCAL_PATH := $(call my-dir)

include $(CLEAR_VARS)
LOCAL_MODULE := cubcoder
LOCAL_SRC_FILES := gomobile/gomobile.go
include $(BUILD_SHARED_LIBRARY)
