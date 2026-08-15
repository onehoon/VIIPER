//go:build cgo && viiper_testbridge

package main

/*
#include <stdint.h>

typedef uintptr_t SteamDeckDeviceHandle;
typedef void (*SteamDeckOutputCallback)(SteamDeckDeviceHandle handle, const uint8_t* data, uint32_t length);

static uint8_t testSteamDeckData[64];
static uint32_t testSteamDeckLength;
static SteamDeckDeviceHandle testSteamDeckHandle;

static void testSteamDeckCallback(SteamDeckDeviceHandle handle, const uint8_t* data, uint32_t length) {
	testSteamDeckHandle = handle;
	testSteamDeckLength = length;
	if (length > sizeof(testSteamDeckData)) {
		length = sizeof(testSteamDeckData);
	}
	for (uint32_t i = 0; i < length; i++) {
		testSteamDeckData[i] = data[i];
	}
}

static SteamDeckOutputCallback getTestSteamDeckCallback(void) {
	return testSteamDeckCallback;
}

static void resetTestSteamDeckCallback(void) {
	testSteamDeckHandle = 0;
	testSteamDeckLength = 0;
	for (uint32_t i = 0; i < sizeof(testSteamDeckData); i++) {
		testSteamDeckData[i] = 0;
	}
}

static SteamDeckDeviceHandle getTestSteamDeckHandle(void) {
	return testSteamDeckHandle;
}

static uint32_t getTestSteamDeckLength(void) {
	return testSteamDeckLength;
}

static const uint8_t* getTestSteamDeckData(void) {
	return testSteamDeckData;
}
*/
import "C"

import (
	"unsafe"
)

func registerTestSteamDeckCOutputCallback(handle uintptr) bool {
	return SetSteamDeckOutputCallback(C.SteamDeckDeviceHandle(handle), C.SteamDeckOutputCallback(C.getTestSteamDeckCallback()))
}

func clearTestSteamDeckCOutputCallback(handle uintptr) bool {
	return SetSteamDeckOutputCallback(C.SteamDeckDeviceHandle(handle), nil)
}

func resetTestSteamDeckCOutputCallback() {
	C.resetTestSteamDeckCallback()
}

func testSteamDeckCOutputSnapshot() (uintptr, uint32, []byte) {
	length := uint32(C.getTestSteamDeckLength())
	if length > 64 {
		length = 64
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(C.getTestSteamDeckData())), int(length))
	return uintptr(C.getTestSteamDeckHandle()), length, append([]byte(nil), data...)
}
