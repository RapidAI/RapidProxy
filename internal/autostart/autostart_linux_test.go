//go:build linux

package autostart

import (
	"strings"
	"testing"
)

func TestDesktopEntry(t *testing.T) {
	got := desktopEntry(`"/opt/rapidproxy/rapidproxy" --hidden`)
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Exec=\"/opt/rapidproxy/rapidproxy\" --hidden",
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("desktop 条目缺少 %q\n%s", want, got)
		}
	}
}
