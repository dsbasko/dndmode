//go:build darwin
// +build darwin

#import <Cocoa/Cocoa.h>

// cocoa_beep plays the system alert sound. See the Beep doc comment in
// beep_darwin.go for why watch mode needs an audible failure signal and why
// nothing on the unlock path may ever use one.
void cocoa_beep(void) {
    NSBeep();
}
