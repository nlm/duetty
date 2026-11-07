package config

import "strings"

// langInfo holds a Whisper language's English name and, when known, its
// native display name.
type langInfo struct{ english, native string }

// whisperLanguages mirrors the LANGUAGES dict of OpenAI Whisper's tokenizer
// (99 codes of the original release plus "yue" added with large-v3).
var whisperLanguages = map[string]langInfo{
	"en": {"english", "English"}, "zh": {"chinese", "中文"}, "de": {"german", "Deutsch"},
	"es": {"spanish", "Español"}, "ru": {"russian", "Русский"}, "ko": {"korean", "한국어"},
	"fr": {"french", "Français"}, "ja": {"japanese", "日本語"}, "pt": {"portuguese", "Português"},
	"tr": {"turkish", "Türkçe"}, "pl": {"polish", "Polski"}, "ca": {"catalan", "Català"},
	"nl": {"dutch", "Nederlands"}, "ar": {"arabic", "العربية"}, "sv": {"swedish", "Svenska"},
	"it": {"italian", "Italiano"}, "id": {"indonesian", "Bahasa Indonesia"}, "hi": {"hindi", "हिन्दी"},
	"fi": {"finnish", "Suomi"}, "vi": {"vietnamese", "Tiếng Việt"}, "he": {"hebrew", "עברית"},
	"uk": {"ukrainian", "Українська"}, "el": {"greek", "Ελληνικά"}, "ms": {"malay", "Bahasa Melayu"},
	"cs": {"czech", "Čeština"}, "ro": {"romanian", "Română"}, "da": {"danish", "Dansk"},
	"hu": {"hungarian", "Magyar"}, "ta": {"tamil", "தமிழ்"}, "no": {"norwegian", "Norsk"},
	"th": {"thai", "ไทย"}, "ur": {"urdu", "اردو"}, "hr": {"croatian", "Hrvatski"},
	"bg": {"bulgarian", "Български"}, "lt": {"lithuanian", "Lietuvių"}, "la": {"latin", "Latina"},
	"mi": {"maori", "Māori"}, "ml": {"malayalam", "മലയാളം"}, "cy": {"welsh", "Cymraeg"},
	"sk": {"slovak", "Slovenčina"}, "te": {"telugu", "తెలుగు"}, "fa": {"persian", "فارسی"},
	"lv": {"latvian", "Latviešu"}, "bn": {"bengali", "বাংলা"}, "sr": {"serbian", "Српски"},
	"az": {"azerbaijani", "Azərbaycanca"}, "sl": {"slovenian", "Slovenščina"}, "kn": {"kannada", "ಕನ್ನಡ"},
	"et": {"estonian", "Eesti"}, "mk": {"macedonian", "Македонски"}, "br": {"breton", "Brezhoneg"},
	"eu": {"basque", "Euskara"}, "is": {"icelandic", "Íslenska"}, "hy": {"armenian", "Հայերեն"},
	"ne": {"nepali", "नेपाली"}, "mn": {"mongolian", "Монгол"}, "bs": {"bosnian", "Bosanski"},
	"kk": {"kazakh", "Қазақша"}, "sq": {"albanian", "Shqip"}, "sw": {"swahili", "Kiswahili"},
	"gl": {"galician", "Galego"}, "mr": {"marathi", "मराठी"}, "pa": {"punjabi", "ਪੰਜਾਬੀ"},
	"si": {"sinhala", "සිංහල"}, "km": {"khmer", "ភាសាខ្មែរ"}, "sn": {"shona", ""},
	"yo": {"yoruba", "Yorùbá"}, "so": {"somali", "Soomaali"}, "af": {"afrikaans", "Afrikaans"},
	"oc": {"occitan", "Occitan"}, "ka": {"georgian", "ქართული"}, "be": {"belarusian", "Беларуская"},
	"tg": {"tajik", "Тоҷикӣ"}, "sd": {"sindhi", "سنڌي"}, "gu": {"gujarati", "ગુજરાતી"},
	"am": {"amharic", "አማርኛ"}, "yi": {"yiddish", "ייִדיש"}, "lo": {"lao", "ລາວ"},
	"uz": {"uzbek", "Oʻzbekcha"}, "fo": {"faroese", "Føroyskt"}, "ht": {"haitian creole", "Kreyòl ayisyen"},
	"ps": {"pashto", "پښتو"}, "tk": {"turkmen", "Türkmençe"}, "nn": {"nynorsk", "Nynorsk"},
	"mt": {"maltese", "Malti"}, "sa": {"sanskrit", "संस्कृतम्"}, "lb": {"luxembourgish", "Lëtzebuergesch"},
	"my": {"myanmar", "မြန်မာ"}, "bo": {"tibetan", "བོད་སྐད་"}, "tl": {"tagalog", "Tagalog"},
	"mg": {"malagasy", "Malagasy"}, "as": {"assamese", "অসমীয়া"}, "tt": {"tatar", "Татарча"},
	"haw": {"hawaiian", "ʻŌlelo Hawaiʻi"}, "ln": {"lingala", "Lingála"}, "ha": {"hausa", "Hausa"},
	"ba": {"bashkir", "Башҡортса"}, "jw": {"javanese", "Basa Jawa"}, "su": {"sundanese", "Basa Sunda"},
	"yue": {"cantonese", "粵語"},
}

// WhisperSupported reports whether code is a language Whisper recognises.
func WhisperSupported(code string) bool {
	_, ok := whisperLanguages[code]
	return ok
}

// LanguageName returns the native display name of a language code, falling
// back to the capitalised English name, and to the code itself if unknown.
func LanguageName(code string) string {
	l, ok := whisperLanguages[code]
	if !ok {
		return code
	}
	if l.native != "" {
		return l.native
	}
	return EnglishName(code)
}

// LanguageCode maps a Whisper language code ("fr") or English name
// ("french", "French") to its code, case-insensitively. ok is false when s
// is not a known language.
func LanguageCode(s string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	if _, ok := whisperLanguages[k]; ok {
		return k, true
	}
	for code, l := range whisperLanguages {
		if l.english == k {
			return code, true
		}
	}
	return "", false
}

// EnglishName returns the capitalised English name of a language code
// ("fr" → "French"), or the code itself if unknown. Used in LLM prompts.
func EnglishName(code string) string {
	l, ok := whisperLanguages[code]
	if !ok {
		return code
	}
	return strings.ToUpper(l.english[:1]) + l.english[1:]
}
