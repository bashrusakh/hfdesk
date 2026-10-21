// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"reflect"
	"regexp"
	"strings"
	"time"
)

// RedactedMarker replaces every value that matches a redaction rule.
const RedactedMarker = "[REDACTED]"

// sensitiveKeyParts are lowercase alphanumeric fragments that mark an
// attribute key as credential bearing. The key is normalized (lowercased,
// every non alphanumeric character removed) before the fragments are matched,
// so "Proxy_Password", "proxy-password" and "proxyPassword" are all treated
// the same.
//
// The rule set is deliberately conservative: it prefers dropping a value that
// turns out to be harmless over leaking a credential.
var sensitiveKeyParts = []string{
	"token",         // hf_token, access_token, api_token
	"password",      // password, proxy_password
	"passwd",        // passwd
	"secret",        // secret, client_secret
	"apikey",        // api_key, apiKey, x-api-key
	"authorization", // authorization, proxy_authorization
	"credential",    // credential, credentials
	"cookie",        // cookie, set_cookie
	"privatekey",    // private_key
	"accesskey",     // access_key
	"sessionid",     // session_id
	"bearer",        // bearer
}

// IsSensitiveKey reports whether an attribute key names a credential and its
// value must therefore never be emitted.
func IsSensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	if normalized == "" {
		return false
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

// normalizeKey lowercases a key and removes every character that is not a
// letter or a digit.
func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// The message level rules below are an explicit, bounded rule set. Each rule
// matches one shape that this codebase can realistically produce.
var (
	// ruleURLCredentials matches credentials embedded in a URL, for example
	// "http://user:password@proxy.example:8080".
	ruleURLCredentials = regexp.MustCompile(
		`(?P<scheme>[a-zA-Z][a-zA-Z0-9+.\-]*://)(?P<user>[^/\s:@]+):(?P<password>[^/\s@]+)@`)

	// ruleAuthorization matches an Authorization or Proxy-Authorization
	// header value, with or without a scheme.
	ruleAuthorization = regexp.MustCompile(
		`(?i)(?P<key>proxy-authorization|authorization)(?P<sep>\s*[:=]\s*)(?P<quote>"?)(?P<scheme>bearer\s+|basic\s+)?(?P<value>[^\s"',;]+)`)

	// ruleBasicBearer matches a bare Basic or Bearer credential that is not
	// introduced by an Authorization key.
	ruleBasicBearer = regexp.MustCompile(
		`(?i)\b(?P<scheme>basic|bearer)\s+(?P<value>[A-Za-z0-9+/=_\-]{8,})`)

	// ruleKeyValue matches "<sensitive key><separator><value>" pairs. The key
	// list is closed: it covers the credential names HFDesk stores or
	// forwards, and nothing else.
	ruleKeyValue = regexp.MustCompile(
		`(?i)(?P<key>\b(?:hf[_-]?token|access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?token|api[_-]?key|apikey|x-api-key|token|authorization|proxy[_-]?password|password|passwd|pwd|secret|client[_-]?secret|credential|credentials|cookie|private[_-]?key|access[_-]?key|secret[_-]?key)\b)(?P<sep>\s*[:=]\s*)(?P<quote>"?)(?P<value>[^\s"',;]+)`)

	// ruleHFToken matches a Hugging Face style token, "hf_" followed by at
	// least ten alphanumeric characters.
	ruleHFToken = regexp.MustCompile(`\bhf_[A-Za-z0-9]{10,}\b`)
)

// Redact removes credentials from free text. It is applied to every record
// message and to every string attribute value before the record reaches a
// sink.
//
// The rule set is:
//
//  1. credentials embedded in a URL: "scheme://user:password@host" keeps
//     scheme and user, the password is replaced.
//  2. an Authorization or Proxy-Authorization value: the credential is
//     replaced, the scheme ("Bearer", "Basic") is kept.
//  3. a bare Basic or Bearer credential of at least eight characters.
//  4. "<sensitive key><: or =><value>" pairs for a closed list of credential
//     names (token, api key, password, secret, credential, cookie, ...).
//  5. a Hugging Face style token, "hf_" followed by at least ten
//     alphanumeric characters.
//
// Everything that does not match one of those rules is left untouched.
func Redact(text string) string {
	if text == "" {
		return ""
	}
	out := text
	out = ruleURLCredentials.ReplaceAllString(out, "${scheme}${user}:"+RedactedMarker+"@")
	out = ruleAuthorization.ReplaceAllString(out, "${key}${sep}${quote}${scheme}"+RedactedMarker+"${quote}")
	out = ruleBasicBearer.ReplaceAllString(out, "${scheme} "+RedactedMarker)
	out = ruleKeyValue.ReplaceAllString(out, "${key}${sep}${quote}"+RedactedMarker+"${quote}")
	out = ruleHFToken.ReplaceAllString(out, RedactedMarker)
	return out
}

// redactAttrs returns a copy of attrs with credential bearing keys replaced
// by the redaction marker and every remaining value redacted.
func redactAttrs(attrs map[string]any) map[string]any {
	out := make(map[string]any, len(attrs))
	for key, value := range attrs {
		if IsSensitiveKey(key) {
			out[key] = RedactedMarker
			continue
		}
		out[key] = redactValue(value)
	}
	return out
}

// redactValue redacts a single attribute value.
//
// Strings are scrubbed. Maps, slices and structs are converted into a
// generic redacted form, because a credential can hide in a nested field of a
// configuration or request structure.
func redactValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return Redact(typed)
	case error:
		return Redact(typed.Error())
	case map[string]any:
		return redactAttrs(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactValue(item)
		}
		return out
	default:
		return redactReflect(reflect.ValueOf(value))
	}
}

// timeType is the type of a time.Time, which is kept as it is.
var timeType = reflect.TypeOf(time.Time{})

// redactReflect walks a value through reflection and returns a redacted
// generic copy of it.
func redactReflect(value reflect.Value) any {
	switch value.Kind() {
	case reflect.String:
		return Redact(value.String())
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return value.Interface()
		}
		out := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			if IsSensitiveKey(key) {
				out[key] = RedactedMarker
				continue
			}
			out[key] = redactReflect(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			// A byte slice is a payload, not a credential carrier.
			return value.Interface()
		}
		out := make([]any, 0, value.Len())
		for i := range value.Len() {
			out = append(out, redactReflect(value.Index(i)))
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return redactReflect(value.Elem())
	case reflect.Struct:
		return redactStruct(value)
	default:
		return value.Interface()
	}
}

// redactStruct converts a struct into a generic map and redacts its fields.
//
// The field names are the ones of the JSON encoding, because those are the
// names that reach the sinks. Unexported fields are skipped: their content
// never reaches a sink.
func redactStruct(value reflect.Value) any {
	if value.Type() == timeType {
		return value.Interface()
	}
	out := make(map[string]any, value.NumField())
	structType := value.Type()
	for i := range value.NumField() {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		name, skip := jsonName(field)
		if skip {
			continue
		}
		if IsSensitiveKey(name) {
			out[name] = RedactedMarker
			continue
		}
		out[name] = redactReflect(value.Field(i))
	}
	return out
}

// jsonName returns the name a struct field has in its JSON encoding and
// reports whether the field is skipped by the encoding.
func jsonName(field reflect.StructField) (string, bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, false
	}
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "-":
		return "", true
	case "":
		return field.Name, false
	default:
		return name, false
	}
}
