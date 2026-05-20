// ua.go — User-Agent パーサ (標準ライブラリのみ)
//
// Step 03 と同じ。
package main

import "strings"

// ParseUA は User-Agent から (device, os, browser) を推定する。
// 不明な場合は空文字を返す。
func ParseUA(ua string) (device, osName, browser string) {
	ua = strings.ToLower(ua)

	switch {
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		osName = "iOS"
	case strings.Contains(ua, "android"):
		osName = "Android"
	case strings.Contains(ua, "mac os x"), strings.Contains(ua, "macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "windows"):
		osName = "Windows"
	case strings.Contains(ua, "linux"):
		osName = "Linux"
	}

	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"):
		device = "tablet"
	case strings.Contains(ua, "mobile"),
		strings.Contains(ua, "iphone"),
		strings.Contains(ua, "android"):
		device = "mobile"
	default:
		device = "desktop"
	}

	switch {
	case strings.Contains(ua, "edg/"), strings.Contains(ua, "edge/"):
		browser = "Edge"
	case strings.Contains(ua, "opr/"), strings.Contains(ua, "opera"):
		browser = "Opera"
	case strings.Contains(ua, "firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}

	return
}
