package web

import (
	"fmt"
	"strings"
)

type themeColors struct {
	Bg, Surface, Surface2, Border, Text, Muted, Accent, OnAccent, Soft string
	Ok, Warn, Crit, CritSoft, Track, Shadow                            string
}

type theme struct {
	ID     string
	Label  string
	Dark   bool
	Pair   string // the light/dark counterpart, "" if none
	Colors themeColors
}

const defaultTheme = "catppuccin"

func themeByID(id string) (theme, bool) {
	for _, t := range themes {
		if t.ID == id {
			return t, true
		}
	}
	return theme{}, false
}

func themeOrDefault(id string) theme {
	if t, ok := themeByID(id); ok {
		return t
	}
	t, _ := themeByID(defaultTheme)
	return t
}

// systemPair returns the light and dark themes used when following the device setting.
func systemPair(id string) (light, dark theme) {
	t := themeOrDefault(id)
	other, ok := themeByID(t.Pair)
	switch {
	case t.Dark && ok:
		return other, t
	case t.Dark:
		return themeOrDefault("latte"), t
	case ok:
		return t, other
	default:
		return t, themeOrDefault("catppuccin")
	}
}

// nextTheme cycles through themes of the same brightness, so the header
// button never flips a dark-mode user into a light theme.
func nextTheme(id string) string {
	cur := themeOrDefault(id)
	var same []theme
	for _, t := range themes {
		if t.Dark == cur.Dark {
			same = append(same, t)
		}
	}
	for i, t := range same {
		if t.ID == cur.ID {
			return same[(i+1)%len(same)].ID
		}
	}
	return defaultTheme
}

// themeCSS renders one [data-theme] block per theme.
func themeCSS() string {
	var b strings.Builder
	for _, t := range themes {
		c := t.Colors
		scheme := "light"
		if t.Dark {
			scheme = "dark"
		}
		fmt.Fprintf(&b, "[data-theme=%q]{color-scheme:%s;--bg:%s;--surface:%s;--surface2:%s;--border:%s;--text:%s;--muted:%s;"+
			"--accent:%s;--on-accent:%s;--soft:%s;--ok:%s;--warn:%s;--crit:%s;--crit-soft:%s;--track:%s;--shadow:%s}\n",
			t.ID, scheme, c.Bg, c.Surface, c.Surface2, c.Border, c.Text, c.Muted, c.Accent, c.OnAccent, c.Soft,
			c.Ok, c.Warn, c.Crit, c.CritSoft, c.Track, c.Shadow)
	}
	return b.String()
}
