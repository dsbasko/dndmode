// +build darwin

#import <Foundation/Foundation.h>

// permissions_os_version reads NSProcessInfo.operatingSystemVersion and
// writes the three components into the provided long pointers.
//
// Self-contained @autoreleasepool — the caller (Go side) has no Objective-C
// memory-management context and must not be expected to drain a pool. This
// pattern is the project convention (mitigation): every.m
// function that allocates Foundation objects owns its own autoreleasepool.
//
// The values are written via out-parameters because cgo's struct return is
// noisier than three primitive writes; the Go wrapper recomposes them into
// the OSVersion struct.
void permissions_os_version(long *major, long *minor, long *patch) {
    @autoreleasepool {
        NSOperatingSystemVersion v = [[NSProcessInfo processInfo] operatingSystemVersion];
        *major = (long)v.majorVersion;
        *minor = (long)v.minorVersion;
        *patch = (long)v.patchVersion;
    }
}
