// Stand-in for the one Foundation type internal/macos/cocoa/terminalcorpus_darwin.h
// uses, so terminalcorpus_linux.c can compile that header as plain C. The
// corpus stays a single file shared by both platforms.
#ifndef DNDMODE_COMPAT_FOUNDATION_H
#define DNDMODE_COMPAT_FOUNDATION_H
typedef long NSInteger;
#endif
