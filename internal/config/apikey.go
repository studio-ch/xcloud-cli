package config

import "regexp"

// keyPattern is a straight port of extractPrefix() in
// apps/api/src/lib/api-keys.ts. It MUST stay byte-for-byte equivalent:
// `auth status` matches the locally derived prefix against the `prefix`
// column returned by GET /v1/api-keys to identify which key is in use,
// and a mismatch would silently report the wrong key's scopes.
//
// The trap this guards against: the key is `sk_live_` + 32 random bytes
// base64url, and base64url's alphabet itself contains `_` and `-`. There
// is no second underscore separator, so splitting on "_" is wrong — the
// lookup prefix is positional, the first 12 characters after `sk_live_`.
var keyPattern = regexp.MustCompile(`^(sk_(?:live|test))_([A-Za-z0-9_-]{12})`)

// ExtractKeyPrefix returns the O(1) lookup prefix for a full API key, or
// "" if the value is not shaped like one.
func ExtractKeyPrefix(fullKey string) string {
	m := keyPattern.FindStringSubmatch(fullKey)
	if m == nil {
		return ""
	}
	return m[1] + "_" + m[2]
}

// LooksLikeAPIKey reports whether a string has the shape of a Cloud
// Console API key. Used to give a precise error at login time ("that
// does not look like an API key") instead of letting the user discover
// it as a 401 three commands later.
func LooksLikeAPIKey(s string) bool {
	return ExtractKeyPrefix(s) != ""
}
