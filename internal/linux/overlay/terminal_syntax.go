//go:build linux

package overlay

import "strings"

// Syntax highlighting for the terminal style: a byte-for-byte Go port of
// term_tokenize and its language table from the macOS TerminalView
// (internal/macos/cocoa/terminalview_darwin.m). The tokenizer tests are the
// macOS ones (terminal_syntax_test.go), so the two implementations are
// pinned to the same segments. Keyword tables are copied verbatim; the
// comments explaining each choice live in the macOS file.

// termTokClass is a colour class. The order matches TermClass on macOS.
type termTokClass int

const (
	termClassIdent termTokClass = iota
	termClassKeyword
	termClassString
	termClassComment
	termClassNumber
	termClassPunct
)

// termPalette is the dark-editor palette, indexed by termTokClass.
var termPalette = []dvdRGB{
	{0.80, 0.82, 0.85}, // ident: soft off-white
	{0.65, 0.55, 0.95}, // keyword: violet
	{0.45, 0.80, 0.45}, // string: green
	{0.40, 0.42, 0.45}, // comment: dim gray
	{0.90, 0.65, 0.35}, // number: amber
	{0.60, 0.62, 0.65}, // punct: muted gray
}

// termCaretColor is the pale-green block cursor.
var termCaretColor = dvdRGB{0.90, 1.00, 0.90}

// Comment markers, as a bitmask (MySQL accepts two).
const (
	termCommentSlash     = 1 << iota // "//" to end of line
	termCommentHash                  // "#" to end of line
	termCommentDash                  // "--" to end of line
	termCommentParenStar             // "(*" to "*)"
)

// termSyntax parameterises the tokenizer per language.
type termSyntax struct {
	keywords          []string
	extraKeywords     []string // the vendor table layered over ANSI SQL
	comments          int
	singleQuoteString bool
	identStartExtra   string // extra bytes that may start a word
	identCharExtra    string // extra bytes that may continue one
}

// termColSegment is one tokenized run: bytes [start, start+length) of the
// line, occupying display columns [col, col+cols). Bytes and columns differ
// for the Cyrillic ys corpus, where one glyph is two bytes.
type termColSegment struct {
	start  int
	length int
	col    int
	cols   int
	cls    termTokClass
}

// termSegment is the byte half of termColSegment (for the ASCII tests).
type termSegment struct {
	start  int
	length int
	cls    termTokClass
}

// termLangs maps a terminal_language value to its index in the C corpus
// (terminalcorpus_linux.c) and its syntax. Unknown values fall back to Go,
// like config.NormalizeTerminalLanguage.
var termLangs = map[string]int{
	"go": 0, "python": 1, "typescript": 2, "rust": 3, "ys": 4,
	"sql": 5, "psql": 6, "mysql": 7, "oracle": 8, "varaq": 9, "elvish": 10,
	"php": 11,
}

// termLangIndex resolves a language name, defaulting to Go.
func termLangIndex(lang string) int {
	if i, ok := termLangs[lang]; ok {
		return i
	}
	return 0
}

// termSyntaxes is indexed like termLangs.
var termSyntaxes = []termSyntax{
	{keywords: termGoKeywords, comments: termCommentSlash},
	{keywords: termPyKeywords, comments: termCommentHash, singleQuoteString: true},
	{keywords: termTsKeywords, comments: termCommentSlash, singleQuoteString: true},
	{keywords: termRsKeywords, comments: termCommentSlash},
	{keywords: termYsKeywords, comments: termCommentSlash, singleQuoteString: true},
	{keywords: termSqlKeywords, comments: termCommentDash, singleQuoteString: true},
	{keywords: termSqlKeywords, extraKeywords: termPgKeywords, comments: termCommentDash, singleQuoteString: true},
	{keywords: termSqlKeywords, extraKeywords: termMyKeywords, comments: termCommentDash | termCommentHash, singleQuoteString: true},
	{keywords: termSqlKeywords, extraKeywords: termOraKeywords, comments: termCommentDash, singleQuoteString: true},
	{keywords: termVqKeywords, comments: termCommentParenStar, identStartExtra: "'"},
	{keywords: termElvKeywords, comments: termCommentHash, singleQuoteString: true, identCharExtra: "-"},
	{keywords: termPhpKeywords, comments: termCommentSlash | termCommentHash, singleQuoteString: true},
}

func termIsUTF8Cont(b byte) bool { return b&0xC0 == 0x80 }

// termColsBetween counts glyphs in text[from:to].
func termColsBetween(text string, from, to int) int {
	n := 0
	for k := from; k < to; k++ {
		if !termIsUTF8Cont(text[k]) {
			n++
		}
	}
	return n
}

// termBytesForCols returns the byte length of the first cols glyphs of
// text[start:start+maxLen], never splitting a glyph.
func termBytesForCols(text string, start, maxLen, cols int) int {
	k, seen := 0, 0
	for k < maxLen {
		if !termIsUTF8Cont(text[start+k]) {
			if seen >= cols {
				break
			}
			seen++
		}
		k++
	}
	return k
}

func termIsIdentStart(c byte) bool {
	return c >= 0x80 || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}
func termIsDigit(c byte) bool     { return c >= '0' && c <= '9' }
func termIsIdentChar(c byte) bool { return termIsIdentStart(c) || termIsDigit(c) }

func termInSet(c byte, set string) bool { return c != 0 && strings.IndexByte(set, c) >= 0 }

func (s *termSyntax) identStart(c byte) bool {
	return termIsIdentStart(c) || termInSet(c, s.identStartExtra)
}

func (s *termSyntax) identChar(c byte) bool {
	return termIsIdentChar(c) || termInSet(c, s.identStartExtra) || termInSet(c, s.identCharExtra)
}

func (s *termSyntax) isKeyword(word string) bool {
	for _, tbl := range [][]string{s.keywords, s.extraKeywords} {
		for _, k := range tbl {
			if k == word {
				return true
			}
		}
	}
	return false
}

// commentEnd returns where a comment starting at text[i] ends (exclusive),
// or -1 when none starts there.
func (s *termSyntax) commentEnd(text string, i int) int {
	n := len(text)
	c := text[i]
	if s.comments&termCommentHash != 0 && c == '#' {
		return n
	}
	if i+1 >= n {
		return -1
	}
	d := text[i+1]
	if c == d && ((s.comments&termCommentSlash != 0 && c == '/') || (s.comments&termCommentDash != 0 && c == '-')) {
		return n
	}
	if s.comments&termCommentParenStar != 0 && c == '(' && d == '*' {
		for k := i + 2; k+1 < n; k++ {
			if text[k] == '*' && text[k+1] == ')' {
				return k + 2
			}
		}
		return n
	}
	return -1
}

// tokenize splits a line into coloured segments covering it gap-free.
func (s *termSyntax) tokenize(text string) []termColSegment {
	n := len(text)
	var segs []termColSegment
	i, col := 0, 0
	emit := func(end int, cls termTokClass) {
		w := termColsBetween(text, i, end)
		segs = append(segs, termColSegment{start: i, length: end - i, col: col, cols: w, cls: cls})
		col += w
		i = end
	}
	for i < n {
		c := text[i]
		if end := s.commentEnd(text, i); end >= 0 {
			emit(end, termClassComment)
			continue
		}
		if c == '"' || (s.singleQuoteString && c == '\'') {
			j := i + 1
			for j < n && text[j] != c {
				if text[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			if j < n {
				j++
			}
			emit(j, termClassString)
			continue
		}
		if termIsDigit(c) {
			j := i + 1
			for j < n && (termIsIdentChar(text[j]) || text[j] == '.') {
				j++
			}
			emit(j, termClassNumber)
			continue
		}
		if s.identStart(c) {
			j := i + 1
			for j < n && s.identChar(text[j]) {
				j++
			}
			cls := termClassIdent
			if s.isKeyword(text[i:j]) {
				cls = termClassKeyword
			}
			emit(j, cls)
			continue
		}
		j := i + 1
		for j < n {
			d := text[j]
			if s.identStart(d) || termIsDigit(d) || d == '"' || (s.singleQuoteString && d == '\'') || s.commentEnd(text, j) >= 0 {
				break
			}
			j++
		}
		emit(j, termClassPunct)
	}
	return segs
}

// tokenizeLineColsForTest and the two helpers below keep the names the
// macOS tests call, so those tests run here unchanged.
func tokenizeLineColsForTest(line, lang string) []termColSegment {
	return termSyntaxes[termLangIndex(lang)].tokenize(line)
}

func tokenizeLineForTest(line string) []termSegment {
	cols := tokenizeLineColsForTest(line, "go")
	segs := make([]termSegment, len(cols))
	for i, s := range cols {
		segs[i] = termSegment{start: s.start, length: s.length, cls: s.cls}
	}
	return segs
}

func revealBytesForTest(line string, cols int) int {
	return termBytesForCols(line, 0, len(line), cols)
}

// Keyword tables, verbatim from terminalview_darwin.m.

var termGoKeywords = []string{
	"func", "return", "if", "else", "for", "range", "var", "const", "type",
	"struct", "import", "package", "switch", "case", "default", "break",
	"continue", "go", "defer", "chan", "map", "interface", "select", "nil",
	"true", "false", "int",
}

var termPyKeywords = []string{
	"def", "class", "return", "if", "elif", "else", "for", "while", "break",
	"continue", "pass", "import", "from", "as", "with", "try", "except",
	"finally", "raise", "lambda", "yield", "None", "True", "False", "and", "or",
	"not", "in", "is", "global", "nonlocal", "async", "await", "del", "assert",
	"self", "int", "str", "float", "bool", "list", "dict",
}

var termTsKeywords = []string{
	"function", "return", "if", "else", "for", "while", "do", "break",
	"continue", "const", "let", "var", "class", "interface", "type", "enum",
	"extends", "implements", "public", "private", "protected", "readonly",
	"static", "new", "this", "void", "number", "string", "boolean", "null",
	"undefined", "true", "false", "async", "await", "of", "in", "typeof",
	"instanceof", "switch", "case", "default", "try", "catch", "finally",
	"throw", "export", "import", "as",
}

var termRsKeywords = []string{
	"fn", "let", "mut", "const", "static", "struct", "enum", "impl", "trait",
	"pub", "use", "mod", "if", "else", "match", "for", "while", "loop",
	"return", "break", "continue", "ref", "move", "as", "where", "type", "self",
	"Self", "Some", "None", "Ok", "Err", "true", "false", "i32", "u32", "i64",
	"u64", "usize", "isize", "f32", "f64", "bool", "char", "str", "String",
	"Vec", "Option", "Result", "Box", "dyn", "async", "await", "unsafe",
	"crate", "super",
}

var termYsKeywords = []string{
	"йопта", "куку", "гыы", "участковый",
	"ясенХуй", "вилкойвглаз", "иливжопураз",
	"го", "потрещим", "крч", "отвечаю", "харэ",
	"двигай", "естьчо", "лещ", "пахану", "хапнуть",
	"гоп", "тюряжка", "пнх", "гыйбать", "тырыпыры",
	"клево", "батя", "ебнуть", "ассо",
	"сидетьНахуй", "чезажижан", "шкура",
	"чоунастут", "сашаГрей", "яга", "попонятия",
	"мой", "ебанное", "подкрыша", "логопед",
	"хзйопт", "трулио", "чотко", "нетрулио",
	"нечотко", "нуллио", "порожняк", "неибу",
	"нихуя", "жЫ", "есть", "сука", "внатуре", "нах",
	"нахуй", "эквалио", "ровно", "типа",
	"конкретно", "блябуду", "поцик", "поц",
	"пизже", "хуевей", "ичо", "иличо", "чобля",
	"плюсуюНа", "слилсяНа", "пацан", "хуйня",
	"плавник", "колонна", "двойные", "эээ",
	"семка", "Помойка", "Кент", "Гопец",
	"СловоПацана",
}

var termVqKeywords = []string{
	"woD", "latlh", "tam", "chImmoH", "qaw", "qawHa'", "Hotlh", "pong", "cher",
	"HIja'chugh", "ghobe'chugh", "wIv", "chov", "nargh", "vangqa'", "SIj",
	"muv", "ghorqu'", "chIm'a'", "tlheghrar", "naQmoH", "tlheghrap'a'",
	"tlheghpe'", "tlheghjuv", "jor", "boq", "boqHa'", "boq'egh", "boqHa''egh",
	"HabboqHa''egh", "chuv", "boqHa'qa'", "loS'ar", "wa'boq", "wa'boqHa'",
	"joq", "joqHa'", "qojmI'", "qojHa'", "ghurtaH", "maHghurtaH", "poD", "Hab",
	"'ar", "mIScher", "mIS", "HeHmI'", "ghurmI'", "HabmI''a'", "mI''a'",
	"mI'moH", "mobmoH", "DuD", "tlhoch", "Qo'moH", "nIHghoS", "poSghoS",
	"law''a'", "puS'a'", "rap'a'", "law'rap'a'", "puSrap'a'", "rapbe'a'",
	"pagh'a'", "taH'a'", "je", "ghap", "ghobe'", "cha'", "bep", "chu'DonwI'",
	"chu'tut", "pongmI'", "taghDe'",
}

var termElvKeywords = []string{
	"var", "set", "tmp", "del", "fn", "if", "elif", "else", "while", "for",
	"try", "catch", "finally", "and", "or", "coalesce", "pragma", "break",
	"continue", "return", "fail", "put", "echo", "print", "printf", "pprint",
	"repr", "each", "peach", "range", "count", "take", "drop", "keys",
	"has-key", "has-value", "not", "eq", "not-eq", "is", "compare", "order",
	"make-map", "ns", "all", "one", "slurp", "from-lines", "to-lines",
	"from-json", "to-json", "from-terminated", "to-terminated", "nop", "sleep",
	"time", "styled", "true", "false", "nil", "ok",
}

var termSqlKeywords = []string{
	"SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "IN", "IS", "NULL", "AS",
	"ON", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "CROSS", "NATURAL",
	"USING", "GROUP", "BY", "ORDER", "HAVING", "ASC", "DESC", "DISTINCT", "ALL",
	"ANY", "SOME", "EXISTS", "BETWEEN", "LIKE", "CASE", "WHEN", "THEN", "ELSE",
	"END", "UNION", "INTERSECT", "EXCEPT", "WITH", "RECURSIVE", "INSERT",
	"INTO", "VALUES", "UPDATE", "SET", "DELETE", "MERGE", "MATCHED", "CREATE",
	"ALTER", "DROP", "TABLE", "VIEW", "INDEX", "UNIQUE", "PRIMARY", "KEY",
	"FOREIGN", "REFERENCES", "CONSTRAINT", "CHECK", "DEFAULT", "ADD", "COLUMN",
	"CASCADE", "RESTRICT", "REPLACE", "TRIGGER", "BEFORE", "AFTER", "INSTEAD",
	"EACH", "ROW", "ROWS", "RANGE", "PRECEDING", "FOLLOWING", "UNBOUNDED",
	"CURRENT", "OVER", "PARTITION", "WINDOW", "FETCH", "FIRST", "NEXT", "ONLY",
	"OFFSET", "FILTER", "LATERAL", "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT",
	"TRANSACTION", "START", "WORK", "GRANT", "REVOKE", "TO", "TRUE", "FALSE",
	"UNKNOWN", "GENERATED", "ALWAYS", "IDENTITY", "FOR", "OF", "CAST",
	"EXTRACT", "INTERVAL", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP",
	"COALESCE", "NULLIF", "COUNT", "SUM", "AVG", "MIN", "MAX", "ROW_NUMBER",
	"RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST", "NTILE", "LAG", "LEAD",
	"FIRST_VALUE", "LAST_VALUE", "UPPER", "LOWER", "TRIM", "SUBSTRING",
	"POSITION", "CHAR_LENGTH", "ROUND", "ABS", "FLOOR", "CEILING", "MOD",
	"NULLS", "INTEGER", "INT", "SMALLINT", "BIGINT", "DECIMAL", "NUMERIC",
	"REAL", "FLOAT", "DOUBLE", "PRECISION", "CHAR", "CHARACTER", "VARCHAR",
	"VARYING", "TEXT", "BOOLEAN", "DATE", "TIME", "TIMESTAMP", "ZONE", "YEAR",
	"MONTH", "DAY", "HOUR", "MINUTE", "SECOND", "FUNCTION", "PROCEDURE",
	"RETURNS", "RETURN", "DECLARE", "EXECUTE", "CALL", "IF", "LOOP", "WHILE",
	"TEMPORARY",
}

var termPgKeywords = []string{
	"RETURNING", "CONFLICT", "DO", "NOTHING", "EXCLUDED", "ILIKE", "LIMIT",
	"SERIAL", "BIGSERIAL", "TIMESTAMPTZ", "JSONB", "JSON", "UUID", "BYTEA",
	"INET", "TSVECTOR", "ARRAY", "MATERIALIZED", "REFRESH", "CONCURRENTLY",
	"LANGUAGE", "plpgsql", "VOLATILE", "STABLE", "IMMUTABLE", "STRICT", "RAISE",
	"EXCEPTION", "NOTICE", "PERFORM", "NEW", "OLD", "FOUND", "ELSIF", "VACUUM",
	"ANALYZE", "EXPLAIN", "BUFFERS", "TYPE", "ENUM", "DOMAIN", "EXCLUDE",
	"GIST", "GIN", "INCLUDE", "SETOF", "RECORD", "TABLESAMPLE",
}

var termMyKeywords = []string{
	"AUTO_INCREMENT", "UNSIGNED", "TINYINT", "MEDIUMINT", "DATETIME",
	"MEDIUMTEXT", "LONGTEXT", "ENUM", "JSON", "ENGINE", "CHARSET", "COLLATE",
	"DUPLICATE", "IGNORE", "LIMIT", "IFNULL", "GROUP_CONCAT", "SEPARATOR",
	"CONCAT", "DATE_FORMAT", "DATE_SUB", "DATE_ADD", "NOW", "TIMESTAMPDIFF",
	"STR_TO_DATE", "UNIX_TIMESTAMP", "JSON_EXTRACT", "JSON_OBJECT",
	"JSON_ARRAYAGG", "JSON_TABLE", "STORED", "VIRTUAL", "FULLTEXT", "MATCH",
	"AGAINST", "MODE", "STRAIGHT_JOIN", "FORCE", "ALGORITHM", "INPLACE", "SHOW",
	"EXPLAIN", "FORMAT", "DELIMITER", "HANDLER", "CONTINUE", "EXIT", "CURSOR",
	"OPEN", "CLOSE", "DO", "LEAVE", "ITERATE", "ELSEIF", "SIGNAL", "SQLSTATE",
	"MESSAGE_TEXT", "EVENT", "SCHEDULE", "EVERY",
}

var termOraKeywords = []string{
	"VARCHAR2", "NVARCHAR2", "NUMBER", "RAW", "CLOB", "BLOB", "SYSDATE",
	"SYSTIMESTAMP", "DUAL", "ROWNUM", "ROWID", "LEVEL", "PRIOR", "CONNECT",
	"NOCYCLE", "SIBLINGS", "NVL", "NVL2", "DECODE", "TO_CHAR", "TO_DATE",
	"TO_NUMBER", "TRUNC", "ADD_MONTHS", "MONTHS_BETWEEN", "LAST_DAY", "LISTAGG",
	"WITHIN", "KEEP", "PIVOT", "UNPIVOT", "MINUS", "SEQUENCE", "NEXTVAL",
	"CURRVAL", "INCREMENT", "CACHE", "NOCACHE", "EXCEPTION", "OTHERS",
	"NO_DATA_FOUND", "TOO_MANY_ROWS", "DUP_VAL_ON_INDEX", "RAISE",
	"RAISE_APPLICATION_ERROR", "PRAGMA", "AUTONOMOUS_TRANSACTION", "BULK",
	"COLLECT", "FORALL", "EXIT", "ELSIF", "CURSOR", "TYPE", "RECORD", "ROWTYPE",
	"VARRAY", "PLS_INTEGER", "NEW", "OLD", "OUT", "NOCOPY", "IMMEDIATE",
	"DBMS_OUTPUT", "PUT_LINE", "SQL", "ROWCOUNT", "REGEXP_SUBSTR",
	"REGEXP_LIKE", "REGEXP_REPLACE", "GLOBAL", "PRESERVE", "ONLINE", "COMMENT",
}

var termPhpKeywords = []string{
	"function", "fn", "return", "if", "else", "elseif", "foreach", "for",
	"while", "do", "switch", "case", "default", "break", "continue", "match",
	"class", "interface", "trait", "enum", "extends", "implements", "abstract",
	"final", "readonly", "public", "private", "protected", "static", "const",
	"new", "clone", "instanceof", "throw", "try", "catch", "finally", "yield",
	"from", "as", "echo", "print", "isset", "unset", "empty", "list", "array",
	"string", "int", "float", "bool", "void", "mixed", "never", "null", "true",
	"false", "self", "parent", "this", "iterable", "callable", "object", "and",
	"or", "xor", "global",
}
