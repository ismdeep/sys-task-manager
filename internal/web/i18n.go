package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const localeCookieName = "task_manager_locale"
const defaultLocale = "en"

type localeOption struct {
	Code  string
	Label string
}

type pageMeta struct {
	Locale        string
	LocaleOptions []localeOption
	CurrentURL    string
}

type translationEntry map[string]string
type translations map[string]translationEntry

//go:embed templates/*.html locales/*.json
var webFS embed.FS

var supportedLocaleOptions = []localeOption{
	{Code: "en", Label: "English"},
	{Code: "zh-CN", Label: "简体中文"},
	{Code: "zh-TW", Label: "繁體中文"},
}

func loadTranslations() (translations, error) {
	raw, err := webFS.ReadFile("locales/admin.json")
	if err != nil {
		return nil, err
	}

	var out translations
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) newPageMeta(w http.ResponseWriter, r *http.Request) pageMeta {
	locale := s.resolveLocale(r)
	s.persistLocale(w, locale)

	options := make([]localeOption, len(supportedLocaleOptions))
	copy(options, supportedLocaleOptions)

	return pageMeta{
		Locale:        locale,
		LocaleOptions: options,
		CurrentURL:    r.URL.RequestURI(),
	}
}

func (s *Server) persistLocale(w http.ResponseWriter, locale string) {
	http.SetCookie(w, &http.Cookie{
		Name:     localeCookieName,
		Value:    locale,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(365 * 24 * time.Hour),
	})
}

func (s *Server) resolveLocale(r *http.Request) string {
	if locale := normalizeLocale(r.URL.Query().Get("lang")); locale != "" {
		return locale
	}

	if cookie, err := r.Cookie(localeCookieName); err == nil {
		if locale := normalizeLocale(cookie.Value); locale != "" {
			return locale
		}
	}

	if locale := localeFromHeader(r.Header.Get("Accept-Language")); locale != "" {
		return locale
	}

	return defaultLocale
}

func (s *Server) translateForRequest(r *http.Request, key string) string {
	return s.translate(s.resolveLocale(r), key)
}

func (s *Server) translatefForRequest(r *http.Request, key string, args ...any) string {
	return s.translatef(s.resolveLocale(r), key, args...)
}

func (s *Server) translate(locale, key string) string {
	entry, ok := s.i18n[key]
	if !ok {
		return key
	}

	value := entry[normalizeLocale(locale)]
	if value == "" {
		value = entry[defaultLocale]
	}
	if value == "" {
		value = entry["zh-CN"]
	}
	if value == "" {
		value = key
	}
	return value
}

func (s *Server) translatef(locale, key string, args ...any) string {
	return fmt.Sprintf(s.translate(locale, key), args...)
}

func (s *Server) statusLabel(locale, status string) string {
	status = strings.TrimSpace(strings.ToLower(status))
	if status == "" {
		status = "pending"
	}

	switch status {
	case "success", "failed", "running", "pending", "disabled", "never":
		return s.translate(locale, "status."+status)
	default:
		return status
	}
}

func formatTaskTimeout(locale string, timeout time.Duration, translate func(string, string) string) string {
	if timeout <= 0 {
		return translate(locale, "common.no_limit")
	}
	return timeout.String()
}

func normalizeLocale(raw string) string {
	value := strings.TrimSpace(strings.ReplaceAll(strings.ToLower(raw), "_", "-"))
	switch {
	case value == "en" || strings.HasPrefix(value, "en-"):
		return "en"
	case value == "zh" || value == "zh-cn" || value == "zh-hans" || strings.HasPrefix(value, "zh-cn-") || strings.HasPrefix(value, "zh-hans-"):
		return "zh-CN"
	case value == "zh-tw" || value == "zh-hant" || value == "zh-hk" || value == "zh-mo" || strings.HasPrefix(value, "zh-tw-") || strings.HasPrefix(value, "zh-hant-") || strings.HasPrefix(value, "zh-hk-") || strings.HasPrefix(value, "zh-mo-"):
		return "zh-TW"
	default:
		return ""
	}
}

func localeFromHeader(raw string) string {
	for _, item := range strings.Split(raw, ",") {
		part := strings.TrimSpace(item)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, ";"); idx >= 0 {
			part = part[:idx]
		}
		if locale := normalizeLocale(part); locale != "" {
			return locale
		}
	}
	return ""
}

func withLang(rawURL, locale string) string {
	return withParam(rawURL, "lang", locale)
}

func withParam(rawURL, key, value string) string {
	target, err := url.Parse(rawURL)
	if err != nil {
		return "?" + url.QueryEscape(key) + "=" + url.QueryEscape(value)
	}

	query := target.Query()
	query.Set(key, value)
	target.RawQuery = query.Encode()

	if target.Path == "" {
		target.Path = "/"
	}
	return target.String()
}

func htmlLang(locale string) string {
	switch normalizeLocale(locale) {
	case "zh-CN":
		return "zh-CN"
	case "zh-TW":
		return "zh-TW"
	default:
		return "en"
	}
}
