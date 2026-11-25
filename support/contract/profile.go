package contract

import (
	"encoding/json"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits of a PublicProfile. They are part of the contract so the Hub, the
// management console and the SDK validate the same way.
const (
	ProfileMaxBytes      = 16 * 1024
	ProfileMaxLocales    = 20
	ProfileMaxLauncher   = 60
	ProfileMaxGreeting   = 500
	ProfileMaxOffline    = 500
	ProfileMaxContactURL = 2048

	// Presentation limits of the embed (inclusive; zero means "not set").
	ProfileMaxInlineTarget = 120
	ProfileMinInlineHeight = 320
	ProfileMaxInlineHeight = 1200
	ProfileMinPanelWidth   = 280
	ProfileMaxPanelWidth   = 720
	ProfileMaxOffset       = 120

	BrandColorPattern = `^#[0-9A-Fa-f]{6}$`
	LocalePattern     = `^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`

	// InlineTargetPattern is the closed character set of a simple CSS selector
	// the loader resolves in the host page: no quotes, angle brackets,
	// backslashes or semicolons.
	InlineTargetPattern = `^[A-Za-z0-9_#.:>+~*,=()\[\] -]+$`
)

// Profile appearance and launcher position values.
const (
	AppearanceAuto  = "auto"
	AppearanceLight = "light"
	AppearanceDark  = "dark"

	PositionBottomRight = "bottom-right"
	PositionBottomLeft  = "bottom-left"
)

// Embed display modes. An empty DisplayMode means DisplayFloating.
const (
	DisplayFloating = "floating"
	DisplayInline   = "inline"
	DisplayPush     = "push"
)

// Launcher shapes. An empty LauncherShape means LauncherRound.
const (
	LauncherRound   = "round"
	LauncherRounded = "rounded"
	LauncherSquare  = "square"
)

var (
	brandColorRe = regexp.MustCompile(BrandColorPattern)
	localeRe     = regexp.MustCompile(LocalePattern)

	inlineTargetRe = regexp.MustCompile(InlineTargetPattern)
)

// ProfileTexts are the texts of one locale. They are data, not i18n keys.
type ProfileTexts struct {
	Launcher string `json:"launcher,omitempty"`
	Greeting string `json:"greeting,omitempty"`
	Offline  string `json:"offline,omitempty"`
}

// FallbackContact is what the widget shows when the channel is out of service.
type FallbackContact struct {
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// PublicProfile is the theme and texts of a channel widget. Version is the
// profile version the Hub increments on every change; it is filled by the Hub
// and not validated here. The contrast of BrandColor and the host of LogoURL
// are checked by the Hub, which knows the theme rules and the allowed origins.
type PublicProfile struct {
	Version         int                     `json:"version,omitempty"`
	BrandColor      string                  `json:"brand_color,omitempty"`
	Appearance      string                  `json:"appearance,omitempty"`
	Position        string                  `json:"position,omitempty"`
	LogoURL         string                  `json:"logo_url,omitempty"`
	DefaultLocale   string                  `json:"default_locale,omitempty"`
	Texts           map[string]ProfileTexts `json:"texts,omitempty"`
	FallbackContact *FallbackContact        `json:"fallback_contact,omitempty"`

	// Presentation of the embed. All optional; an empty value keeps the
	// floating launcher. InlineTarget is only a selector the loader resolves
	// in the customer page; the kernel never interprets it.
	DisplayMode   string `json:"display_mode,omitempty"`
	InlineTarget  string `json:"inline_target,omitempty"`
	InlineHeight  int    `json:"inline_height,omitempty"`
	PanelWidth    int    `json:"panel_width,omitempty"`
	Offset        int    `json:"offset,omitempty"`
	LauncherShape string `json:"launcher_shape,omitempty"`
	LauncherLabel string `json:"launcher_label,omitempty"`
}

// Profile violation reasons.
const (
	ProfileReasonInvalid  = "invalid"
	ProfileReasonNotHTTPS = "not_https"
	ProfileReasonTooLong  = "too_long"
	ProfileReasonTooMany  = "too_many"
)

// ProfileError is one violation of a PublicProfile.
type ProfileError struct {
	Field  string
	Reason string
}

func (e ProfileError) Error() string { return e.Field + ": " + e.Reason }

// ProfileErrors is the list of violations returned by Validate.
type ProfileErrors []ProfileError

func (e ProfileErrors) Error() string {
	parts := make([]string, len(e))
	for i, v := range e {
		parts[i] = v.Error()
	}
	return "invalid public profile: " + strings.Join(parts, "; ")
}

// Validate checks the profile against the contract limits and returns every
// violation as a ProfileErrors, or nil. Empty fields are valid (the Hub fills
// defaults).
func (p PublicProfile) Validate() error {
	var errs ProfileErrors
	add := func(field, reason string) { errs = append(errs, ProfileError{field, reason}) }

	if p.BrandColor != "" && !brandColorRe.MatchString(p.BrandColor) {
		add("brand_color", ProfileReasonInvalid)
	}
	switch p.Appearance {
	case "", AppearanceAuto, AppearanceLight, AppearanceDark:
	default:
		add("appearance", ProfileReasonInvalid)
	}
	switch p.Position {
	case "", PositionBottomRight, PositionBottomLeft:
	default:
		add("position", ProfileReasonInvalid)
	}
	switch p.DisplayMode {
	case "", DisplayFloating, DisplayInline, DisplayPush:
	default:
		add("display_mode", ProfileReasonInvalid)
	}
	if p.InlineTarget != "" {
		if len(p.InlineTarget) > ProfileMaxInlineTarget {
			add("inline_target", ProfileReasonTooLong)
		} else if !inlineTargetRe.MatchString(p.InlineTarget) || strings.Contains(strings.ToLower(p.InlineTarget), "javascript:") {
			add("inline_target", ProfileReasonInvalid)
		}
	}
	if p.InlineHeight != 0 && (p.InlineHeight < ProfileMinInlineHeight || p.InlineHeight > ProfileMaxInlineHeight) {
		add("inline_height", ProfileReasonInvalid)
	}
	if p.PanelWidth != 0 && (p.PanelWidth < ProfileMinPanelWidth || p.PanelWidth > ProfileMaxPanelWidth) {
		add("panel_width", ProfileReasonInvalid)
	}
	if p.Offset < 0 || p.Offset > ProfileMaxOffset {
		add("offset", ProfileReasonInvalid)
	}
	switch p.LauncherShape {
	case "", LauncherRound, LauncherRounded, LauncherSquare:
	default:
		add("launcher_shape", ProfileReasonInvalid)
	}
	if utf8.RuneCountInString(p.LauncherLabel) > ProfileMaxLauncher {
		add("launcher_label", ProfileReasonTooLong)
	}
	if p.LogoURL != "" {
		if r := httpsReason(p.LogoURL); r != "" {
			add("logo_url", r)
		}
	}
	if p.DefaultLocale != "" && !localeRe.MatchString(p.DefaultLocale) {
		add("default_locale", ProfileReasonInvalid)
	}
	if len(p.Texts) > ProfileMaxLocales {
		add("texts", ProfileReasonTooMany)
	}
	for loc, t := range p.Texts {
		f := "texts." + loc
		if !localeRe.MatchString(loc) {
			add(f, ProfileReasonInvalid)
			continue
		}
		for _, c := range []struct {
			name, v string
			max     int
		}{{"launcher", t.Launcher, ProfileMaxLauncher}, {"greeting", t.Greeting, ProfileMaxGreeting}, {"offline", t.Offline, ProfileMaxOffline}} {
			if utf8.RuneCountInString(c.v) > c.max {
				add(f+"."+c.name, ProfileReasonTooLong)
			}
		}
	}
	if fc := p.FallbackContact; fc != nil {
		if fc.Email != "" {
			if a, err := mail.ParseAddress(fc.Email); err != nil || a.Address != fc.Email {
				add("fallback_contact.email", ProfileReasonInvalid)
			}
		}
		if fc.URL != "" {
			if len(fc.URL) > ProfileMaxContactURL {
				add("fallback_contact.url", ProfileReasonTooLong)
			} else if r := httpsReason(fc.URL); r != "" {
				add("fallback_contact.url", r)
			}
		}
	}
	if b, err := json.Marshal(p); err == nil && len(b) > ProfileMaxBytes {
		add("profile", ProfileReasonTooLong)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// httpsReason returns "" for an absolute https URL with a host and no
// credentials, and the violation reason otherwise.
func httpsReason(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ProfileReasonInvalid
	}
	if u.Scheme != "https" {
		return ProfileReasonNotHTTPS
	}
	if u.User != nil {
		return ProfileReasonInvalid
	}
	return ""
}
