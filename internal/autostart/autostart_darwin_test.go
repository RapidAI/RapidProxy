//go:build darwin

package autostart

import (
	"strings"
	"testing"
)

func TestPlistXML(t *testing.T) {
	got := plistXML(`"/Applications/RapidProxy.app/Contents/MacOS/RapidProxy" --hidden`)
	for _, want := range []string{
		"<string>RapidProxy</string>",
		"<string>/Applications/RapidProxy.app/Contents/MacOS/RapidProxy</string>",
		"<string>--hidden</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist 内容缺少 %q\n%s", want, got)
		}
	}
}

func TestPlistXMLEscape(t *testing.T) {
	got := plistXML(`/a&b<c>"d" --hidden`)
	if strings.Contains(got, `&b`) && !strings.Contains(got, `&amp;b`) {
		t.Errorf("XML 转义失败:\n%s", got)
	}
}
