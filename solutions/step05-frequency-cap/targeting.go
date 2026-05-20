// targeting.go — StringSet / Targeting / Context / Matcher
//
// Step 03 と同じ。
package main

import (
	"strings"
	"time"
)

// StringSet は include / exclude のいずれかを表現するターゲティングフィールド。
type StringSet struct {
	Mode   string   `json:"mode,omitempty"`
	Values []string `json:"values,omitempty"`
}

func Include(v []string) StringSet { return StringSet{Mode: "include", Values: v} }
func Exclude(v []string) StringSet { return StringSet{Mode: "exclude", Values: v} }

func (s StringSet) Match(v string) bool {
	if s.Mode == "" || len(s.Values) == 0 {
		return true
	}
	v = strings.ToLower(v)
	hit := false
	for _, x := range s.Values {
		if strings.ToLower(x) == v {
			hit = true
			break
		}
	}
	switch s.Mode {
	case "include":
		return hit
	case "exclude":
		return !hit
	default:
		return true
	}
}

// Targeting: 1 LineItem が持つ条件。非空フィールドが AND で評価される。
type Targeting struct {
	Countries StringSet `json:"countries,omitempty"`
	Devices   StringSet `json:"devices,omitempty"`
	OS        StringSet `json:"os,omitempty"`
	Browsers  StringSet `json:"browsers,omitempty"`
	DayOfWeek StringSet `json:"day_of_week,omitempty"`
}

// Context: 1 リクエストから抽出した属性値。
type Context struct {
	Country   string `json:"country,omitempty"`
	Device    string `json:"device,omitempty"`
	OS        string `json:"os,omitempty"`
	Browser   string `json:"browser,omitempty"`
	DayOfWeek string `json:"day_of_week,omitempty"`
}

func dayOfWeekUTC(now time.Time) string {
	return []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}[now.UTC().Weekday()]
}

func (t Targeting) Matches(c Context) bool {
	return t.Countries.Match(c.Country) &&
		t.Devices.Match(c.Device) &&
		t.OS.Match(c.OS) &&
		t.Browsers.Match(c.Browser) &&
		t.DayOfWeek.Match(c.DayOfWeek)
}

func filterByTargeting(items []LineItem, c Context) []LineItem {
	out := make([]LineItem, 0, len(items))
	for _, li := range items {
		if li.Targeting.Matches(c) {
			out = append(out, li)
		}
	}
	return out
}
