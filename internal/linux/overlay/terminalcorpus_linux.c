// Access to the terminal-style corpus. The corpus itself is the macOS
// header, included as is: one source for both platforms.

#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wdeprecated" // its #import line
#include "terminalcorpus_darwin.h"
#pragma GCC diagnostic pop

#include "overlay_linux.h"

// Indexed like termLangs in terminal_syntax.go.
static const TermBlock *term_table(int lang, NSInteger *count) {
	switch (lang) {
	case 1: *count = kPyBlocksCount; return kPyBlocks;
	case 2: *count = kTsBlocksCount; return kTsBlocks;
	case 3: *count = kRsBlocksCount; return kRsBlocks;
	case 4: *count = kYsBlocksCount; return kYsBlocks;
	case 5: *count = kSqlBlocksCount; return kSqlBlocks;
	case 6: *count = kPgBlocksCount; return kPgBlocks;
	case 7: *count = kMyBlocksCount; return kMyBlocks;
	case 8: *count = kOraBlocksCount; return kOraBlocks;
	case 9: *count = kVqBlocksCount; return kVqBlocks;
	case 10: *count = kElvBlocksCount; return kElvBlocks;
	case 11: *count = kPhpBlocksCount; return kPhpBlocks;
	default: *count = kGoBlocksCount; return kGoBlocks;
	}
}

int overlay_term_block_count(int lang) {
	NSInteger n;
	term_table(lang, &n);
	return (int)n;
}

int overlay_term_block_len(int lang, int block) {
	NSInteger n;
	const TermBlock *t = term_table(lang, &n);
	return (block >= 0 && block < n) ? (int)t[block].count : 0;
}

const char *overlay_term_line(int lang, int block, int line) {
	NSInteger n;
	const TermBlock *t = term_table(lang, &n);
	if (block < 0 || block >= n || line < 0 || line >= t[block].count) {
		return "";
	}
	return t[block].lines[line];
}
